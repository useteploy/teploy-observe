"""HTTP client + module-level helpers for the Observe Python SDK."""

from __future__ import annotations

import atexit
import json
import os
import threading
import time
import traceback
from contextlib import contextmanager
from contextvars import ContextVar
from dataclasses import dataclass
from typing import Any, Dict, Iterator, List, Optional
from urllib import request as urlrequest
from urllib.error import URLError


@dataclass
class Options:
    endpoint: str
    api_key: Optional[str] = None
    site_id: str = "default"
    release: Optional[str] = None
    environment: Optional[str] = None
    service_name: Optional[str] = None
    log_batch_size: int = 50
    log_flush_interval: float = 2.0
    timeout: float = 10.0


# Server-side /logs/batch cap; entries beyond it are a 400.
_MAX_LOG_BATCH = 200
# Byte cap per serialized log entry, enforced at admission (audit F36): one
# unserializable or huge entry used to abort the detached batch loop and
# take every valid entry behind it down too.
_MAX_ENTRY_BYTES = 64 * 1024


class Client:
    """Submits events, errors, and logs to Observe.

    Use one Client per process. ``init()`` stores a module-level default so
    the convenience helpers (``info``, ``capture_exception``, etc.) can use it.

    Identity is request-scoped (audit F34): a multi-user threaded or async
    server that shared one mutable ``_distinct_id`` attributed one request's
    errors/logs to whichever user happened to identify last. Use
    ``bind_identity`` around each request; ``identify`` sets the identity for
    the CURRENT context only.
    """

    def __init__(self, **kwargs: Any) -> None:
        if not kwargs.get("endpoint"):
            raise ValueError("observe_sdk: endpoint is required")
        self.opts = Options(**kwargs)
        self._lock = threading.Lock()
        self._buffer: List[Dict[str, Any]] = []
        self._stop = threading.Event()
        # Context-local identity (audit F34): each thread/task sees its own
        # value; unset reads are anonymous. Capture it into a payload BEFORE
        # handing work to a background thread — ContextVar does not transfer.
        self._identity: ContextVar[Optional[str]] = ContextVar(
            f"observe_identity_{id(self)}", default=None
        )
        self._thread = threading.Thread(
            target=self._loop, name="observe-flush", daemon=True
        )
        self._thread.start()

    # ── public API ────────────────────────────────────────────────────────

    @contextmanager
    def bind_identity(self, user_id: Optional[str]) -> Iterator[None]:
        """Bind the identity for the current request/task context.

        Use as a request middleware: ``with client.bind_identity(user.id):``
        — everything logged inside is attributed to that user, and the
        binding is reliably reset on exit even on exceptions.
        """
        token = self._identity.set(str(user_id) if user_id is not None else None)
        try:
            yield
        finally:
            self._identity.reset(token)

    def identify(self, user_id: str, traits: Optional[Dict[str, Any]] = None) -> None:
        """Associate subsequent events / errors / logs with a user identifier
        IN THE CURRENT CONTEXT (audit F34).

        The server hashes ``user_id`` with the per-site ``session_salt`` so
        the raw value never lands in storage (unless the site has the
        ``raw_distinct_id`` opt-in flag set). Emits a one-shot ``$identify``
        ANALYTICS event whose only identity field is the top-level
        ``distinct_id`` the server hashes (audit F33: the old marker was a
        log line carrying a raw ``user_id`` attribute, and the ``distinct_id``
        put on log payloads was silently ignored by the server).
        """
        if not user_id:
            return
        self._identity.set(str(user_id))
        payload: Dict[str, Any] = {
            "site_id": self.opts.site_id,
            "event_type": "$identify",
            "distinct_id": str(user_id),
        }
        if traits:
            payload["properties"] = {
                k: v
                for k, v in traits.items()
                if k not in {"user_id", "distinct_id", "email"}
            }
        self._post("/api/v1/events", payload)

    def reset(self) -> None:
        """Clear the active distinct_id for the current context (e.g. logout)."""
        self._identity.set(None)

    def capture_exception(
        self,
        exc: BaseException,
        *,
        release: Optional[str] = None,
        trace_id: Optional[str] = None,
        span_id: Optional[str] = None,
    ) -> None:
        """Submit a single exception with stack trace. Sends immediately.

        The identity is captured from the CURRENT context before the network
        call so a concurrent reset/identify cannot mislabel it.

        ``trace_id``/``span_id`` attach the active trace context when the
        error was captured inside a traced operation, enabling exact
        trace<->error correlation in the trace detail view.
        """
        payload = {
            "site_id": self.opts.site_id,
            "error_type": type(exc).__name__,
            "error_value": str(exc) or type(exc).__name__,
            "release_tag": release or self.opts.release or "",
            "environment": self.opts.environment or "",
            "level": "error",
            "stack_trace": _stack_frames(exc),
        }
        ident = self._identity.get()
        if ident:
            payload["distinct_id"] = ident
        if trace_id:
            payload["trace_id"] = trace_id
        if span_id:
            payload["span_id"] = span_id
        self._post("/api/v1/errors", payload)

    def log(self, level: str, message: str, **fields: Any) -> None:
        entry = {
            "site_id": self.opts.site_id,
            "level": level,
            "message": message,
            "service_name": self.opts.service_name or "",
            "attributes": fields,
        }
        # Audit F36: validate serialization AT ADMISSION. json.dumps used to
        # run inside the flush loop AFTER the buffer was detached, so one
        # unsupported attribute type raised TypeError out of the loop and
        # silently discarded every valid entry behind it.
        encode_entry(entry)
        with self._lock:
            self._buffer.append(entry)
            full = len(self._buffer) >= self.opts.log_batch_size
        if full:
            self.flush()

    def debug(self, msg: str, **fields: Any) -> None: self.log("debug", msg, **fields)
    def info(self, msg: str, **fields: Any) -> None:  self.log("info", msg, **fields)
    def warn(self, msg: str, **fields: Any) -> None:  self.log("warn", msg, **fields)
    def error(self, msg: str, **fields: Any) -> None: self.log("error", msg, **fields)
    def fatal(self, msg: str, **fields: Any) -> None: self.log("fatal", msg, **fields)

    def flush(self) -> None:
        """Drain the log buffer synchronously via the batch endpoint."""
        while True:
            with self._lock:
                if not self._buffer:
                    return
                batch = self._buffer[:_MAX_LOG_BATCH]
                del self._buffer[:_MAX_LOG_BATCH]
            if batch:
                self._post_batch("/api/v1/logs/batch", batch)

    def close(self) -> None:
        """Stop the background flusher and drain pending logs.

        The join is bounded by the flush interval plus one request timeout
        per remaining batch — a timed-out join still drains synchronously
        afterwards, and any entry that cannot be sent raises instead of
        being reported as success.
        """
        if self._stop.is_set():
            return
        self._stop.set()
        self._thread.join(timeout=self.opts.log_flush_interval + self.opts.timeout + 1.0)
        self.flush()

    # ── internal ──────────────────────────────────────────────────────────

    def _loop(self) -> None:
        while not self._stop.wait(self.opts.log_flush_interval):
            try:
                self.flush()
            except Exception:
                # Never let the flush thread die.
                pass

    def _post(self, path: str, body: Dict[str, Any], *, silent: bool = False) -> None:
        url = self.opts.endpoint.rstrip("/") + path
        data = json.dumps(body).encode("utf-8")
        headers = {"Content-Type": "application/json"}
        if self.opts.api_key:
            headers["X-API-Key"] = self.opts.api_key
        req = urlrequest.Request(url, data=data, headers=headers, method="POST")
        try:
            with urlrequest.urlopen(req, timeout=self.opts.timeout) as resp:
                if resp.status >= 400 and not silent:
                    raise RuntimeError(f"observe: {path} returned {resp.status}")
        except (URLError, TimeoutError) as exc:
            if not silent:
                raise RuntimeError(f"observe: post {path} failed: {exc}") from exc

    def _post_batch(self, path: str, entries: List[Dict[str, Any]]) -> None:
        data = json.dumps({"logs": entries}).encode("utf-8")
        url = self.opts.endpoint.rstrip("/") + path
        headers = {"Content-Type": "application/json"}
        if self.opts.api_key:
            headers["X-API-Key"] = self.opts.api_key
        req = urlrequest.Request(url, data=data, headers=headers, method="POST")
        try:
            with urlrequest.urlopen(req, timeout=self.opts.timeout) as resp:
                if resp.status >= 400:
                    raise RuntimeError(f"observe: {path} returned {resp.status}")
        except (URLError, TimeoutError) as exc:
            raise RuntimeError(f"observe: post {path} failed: {exc}") from exc


def encode_entry(entry: Dict[str, Any]) -> bytes:
    """Serialize one log entry at admission, rejecting what cannot go over
    the wire rather than letting it abort a later detached batch (audit F36).
    NaN/Infinity are rejected too: they are not valid JSON."""
    try:
        raw = json.dumps(entry, allow_nan=False, separators=(",", ":")).encode("utf-8")
    except (TypeError, ValueError) as exc:
        raise ValueError(
            "observe: log entry is not JSON-serializable (attributes must be "
            "JSON types, not NaN/Infinity or arbitrary objects)"
        ) from exc
    if len(raw) > _MAX_ENTRY_BYTES:
        raise ValueError(
            f"observe: log entry exceeds {_MAX_ENTRY_BYTES} bytes after serialization"
        )
    return raw


# ── module-level default client ────────────────────────────────────────────

_default: Optional[Client] = None


def init(**kwargs: Any) -> Client:
    """Initialize the default client. Repeat calls replace the previous one."""
    global _default
    if _default is not None:
        try:
            _default.close()
        except Exception:
            pass
    _default = Client(**kwargs)
    atexit.register(_default.close)
    return _default


def _require() -> Client:
    if _default is None:
        raise RuntimeError("observe_sdk: call init() before using the default client")
    return _default


def close() -> None:
    if _default is not None:
        _default.close()


def flush() -> None:
    _require().flush()


def identify(user_id: str, traits: Optional[Dict[str, Any]] = None) -> None:
    _require().identify(user_id, traits)


def reset() -> None:
    _require().reset()


def capture_exception(
    exc: BaseException,
    *,
    release: Optional[str] = None,
    trace_id: Optional[str] = None,
    span_id: Optional[str] = None,
) -> None:
    _require().capture_exception(exc, release=release, trace_id=trace_id, span_id=span_id)


def log(level: str, message: str, **fields: Any) -> None:
    _require().log(level, message, **fields)


def debug(msg: str, **fields: Any) -> None: _require().debug(msg, **fields)
def info(msg: str, **fields: Any) -> None:  _require().info(msg, **fields)
def warn(msg: str, **fields: Any) -> None:  _require().warn(msg, **fields)
def error(msg: str, **fields: Any) -> None: _require().error(msg, **fields)
def fatal(msg: str, **fields: Any) -> None: _require().fatal(msg, **fields)


# ── stack trace parsing ────────────────────────────────────────────────────

def _stack_frames(exc: BaseException) -> List[Dict[str, Any]]:
    frames: List[Dict[str, Any]] = []
    tb = exc.__traceback__
    if tb is None:
        return frames
    for frame, lineno in traceback.walk_tb(tb):
        code = frame.f_code
        frames.append({
            "function": code.co_name or "<anonymous>",
            "filename": code.co_filename or "<unknown>",
            "lineno": lineno,
            "in_app": _is_in_app(code.co_filename),
        })
    return frames


def _is_in_app(path: str) -> bool:
    if not path:
        return True
    # Heuristic: frames under stdlib / site-packages are not "in app".
    for marker in ("site-packages", "dist-packages", "python3.", "python3/"):
        if marker in path:
            return False
    return True
