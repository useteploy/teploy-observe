"""HTTP client + module-level helpers for the Observe Python SDK."""

from __future__ import annotations

import atexit
import json
import os
import threading
import time
import traceback
from dataclasses import dataclass, field
from typing import Any, Dict, List, Optional
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


class Client:
    """Submits events, errors, and logs to Observe.

    Use one Client per process. ``init()`` stores a module-level default so
    the convenience helpers (``info``, ``capture_exception``, etc.) can use it.
    """

    def __init__(self, **kwargs: Any) -> None:
        if not kwargs.get("endpoint"):
            raise ValueError("observe_sdk: endpoint is required")
        self.opts = Options(**kwargs)
        self._lock = threading.Lock()
        self._buffer: List[Dict[str, Any]] = []
        self._stop = threading.Event()
        # Set by identify(); included verbatim in subsequent payloads.
        # The server hashes it with the per-site session_salt before
        # storage, so the raw value never persists by default.
        self._distinct_id: Optional[str] = None
        self._thread = threading.Thread(
            target=self._loop, name="observe-flush", daemon=True
        )
        self._thread.start()

    # ── public API ────────────────────────────────────────────────────────

    def identify(self, user_id: str, traits: Optional[Dict[str, Any]] = None) -> None:
        """Associate subsequent events / errors / logs with a user identifier.

        The server hashes ``user_id`` with the per-site ``session_salt`` so
        the raw value never lands in storage (unless the site has the
        ``raw_distinct_id`` opt-in flag set).

        ``traits`` are optional and reserved for the future persons UI; for
        now they're attached to the one-shot ``$identify`` log line.
        """
        if not user_id:
            return
        self._distinct_id = str(user_id)
        # Emit a one-shot log line so the server has an explicit identify
        # marker, mirroring the browser SDK's $identify event.
        attrs: Dict[str, Any] = {"user_id": self._distinct_id}
        if traits:
            attrs.update(traits)
        self.log("info", "$identify", **attrs)

    def reset(self) -> None:
        """Clear the active distinct_id (e.g. on logout)."""
        self._distinct_id = None

    def capture_exception(self, exc: BaseException, *, release: Optional[str] = None) -> None:
        """Submit a single exception with stack trace. Sends immediately."""
        payload = {
            "site_id": self.opts.site_id,
            "error_type": type(exc).__name__,
            "error_value": str(exc) or type(exc).__name__,
            "release_tag": release or self.opts.release or "",
            "environment": self.opts.environment or "",
            "level": "error",
            "stack_trace": _stack_frames(exc),
        }
        if self._distinct_id:
            payload["distinct_id"] = self._distinct_id
        self._post("/api/v1/errors", payload)

    def log(self, level: str, message: str, **fields: Any) -> None:
        entry = {
            "site_id": self.opts.site_id,
            "level": level,
            "message": message,
            "service_name": self.opts.service_name or "",
            "attributes": fields,
        }
        if self._distinct_id:
            entry["distinct_id"] = self._distinct_id
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
        """Drain the log buffer synchronously."""
        with self._lock:
            if not self._buffer:
                return
            batch = self._buffer
            self._buffer = []
        for entry in batch:
            self._post("/api/v1/logs", entry, silent=True)

    def close(self) -> None:
        """Stop the background flusher and drain pending logs."""
        if self._stop.is_set():
            return
        self._stop.set()
        self._thread.join(timeout=self.opts.log_flush_interval + 1)
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


def capture_exception(exc: BaseException, *, release: Optional[str] = None) -> None:
    _require().capture_exception(exc, release=release)


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
