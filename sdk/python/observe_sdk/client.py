"""HTTP client + module-level helpers for the Observe Python SDK."""

from __future__ import annotations

import atexit
import json
import math
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
import uuid

# Wire protocol version this SDK speaks (F12/F19 idempotent delivery).
PROTOCOL_VERSION = 2


class _NoTelemetryRedirect(urlrequest.HTTPRedirectHandler):
    """TO-037: never follow a redirect on a credential-bearing request.

    urllib's default redirect follower would forward the X-API-Key header
    to whatever origin the endpoint names; refusing keeps the key on the
    configured endpoint (a deliberate redirect is an operator decision —
    configure the final URL instead).
    """

    def redirect_request(self, req, fp, code, msg, headers, newurl):  # noqa: D102
        return None


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
    # O11 retry budget: retryable failures (429/5xx/network) retry with an
    # immediate first attempt, then exponential backoff off retry_backoff
    # (seconds, 30 s cap) up to max_send_attempts before the chunk is
    # dropped and counted as a loss.
    max_send_attempts: int = 6
    retry_backoff: float = 1.0


# Server-side /logs/batch cap; entries beyond it are a 400.
_MAX_LOG_BATCH = 200
# Byte cap per serialized log entry, enforced at admission (audit F36): one
# unserializable or huge entry used to abort the detached batch loop and
# take every valid entry behind it down too.
_MAX_ENTRY_BYTES = 64 * 1024
# AUD-026/AUD-035 (round 2): pack request bodies by ENCODED BYTES, not
# entry count — 200 near-64-KiB entries exceeded the route's 2 MiB cap and
# the whole batch was rejected. 1 MiB leaves envelope headroom.
_MAX_BATCH_BYTES = 1024 * 1024
# AUD-035: bound total queued bytes so a stalled endpoint cannot grow the
# queue without limit while the worker is blocked.
_MAX_QUEUE_BYTES = 8 * 1024 * 1024


class ObserveHTTPError(RuntimeError):
    """A non-2xx response from the server. ``code`` carries the status so
    callers can classify retryable (429/5xx) from permanent (other 4xx)
    refusals (O11)."""

    def __init__(self, path: str, code: int) -> None:
        super().__init__(f"observe: {path} returned {code}")
        self.code = code


def _retryable(exc: BaseException) -> bool:
    """O11 retry classification: HTTP 429/5xx and transport-level failures
    (URLError, timeouts, resets) are retryable; every other 4xx is
    permanent for that payload."""
    if isinstance(exc, ObserveHTTPError):
        return exc.code == 429 or exc.code >= 500
    return True


def _validate_options(opts: "Options") -> None:
    """AUD-037 (round 2): reject configuration that would spin the worker
    (zero/negative interval), hang requests (non-finite timeout), or break
    batching — BEFORE the thread starts, not at first failure."""
    if type(opts.log_batch_size) is not int or not 1 <= opts.log_batch_size <= _MAX_LOG_BATCH:
        raise ValueError(f"log_batch_size must be an integer in [1, {_MAX_LOG_BATCH}]")
    for name, value, lower, upper in (
        # The interval's upper bound is generous: tests and batch-only
        # deployments pass 3600 to effectively disable the background loop.
        ("log_flush_interval", opts.log_flush_interval, 0.05, 3600.0),
        ("timeout", opts.timeout, 0.1, 60.0),
        ("retry_backoff", opts.retry_backoff, 0.001, 60.0),
    ):
        if isinstance(value, bool) or not isinstance(value, (int, float)) \
                or not math.isfinite(value) or not lower <= value <= upper:
            raise ValueError(f"{name} must be a finite number in [{lower}, {upper}]")
    if type(opts.max_send_attempts) is not int or not 1 <= opts.max_send_attempts <= 100:
        raise ValueError("max_send_attempts must be an integer in [1, 100]")


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
        _validate_options(self.opts)
        self._lock = threading.Lock()
        # F12: stable producer identity for this client instance. Sent with
        # the v2 event envelope so the server can attribute and deduplicate
        # per producer.
        self._producer_id = uuid.uuid4().hex
        # AUD-036 (round 2): the queue holds the bytes encoded AT ADMISSION.
        # Queueing the original dict let callers mutate nested fields after
        # log() returned, silently changing (or poisoning) the eventual body.
        self._buffer: List[bytes] = []
        self._buffer_bytes = 0
        self._flush_lock = threading.Lock()
        self._close_lock = threading.Lock()
        self._state = "open"  # open | closing | closed (AUD-037)
        self._stop = threading.Event()
        self._last_error: Optional[BaseException] = None
        # Optional non-throwing diagnostics hook for background failures
        # (AUD-035): `_loop` used to swallow every exception.
        self.on_error: Optional[Any] = None
        # O11: visible loss/delivery counters, surfaced via stats().
        # dropped maps reason -> count: entry_invalid, entry_oversize,
        # queue_full, retry_exhausted, non_retryable, server_rejected,
        # shutdown_unflushed. delivered + sum(dropped) + len(queue)
        # accounts for every admitted record.
        self._stats_dropped: Dict[str, int] = {}
        self._stats_delivered = 0
        self._stats_retries = 0
        # O11 retry state for the queue head: consecutive attempts and the
        # backoff gate. The first retry is immediate; the gate engages
        # from the second consecutive failure (same policy as the Go SDK).
        self._send_attempts = 0
        self._retry_not_before = 0.0
        # Context-local identity (audit F34): each thread/task sees its own
        # value; unset reads are anonymous. Capture it into a payload BEFORE
        # handing work to a background thread — ContextVar does not transfer.
        self._identity: ContextVar[Optional[str]] = ContextVar(
            f"observe_identity_{id(self)}", default=None
        )
        self._thread = threading.Thread(
            target=self._loop, name="observe-flush", daemon=True
        )
        # TO-037: the opener refuses redirects; a 3xx surfaces as an error
        # (strict 2xx below) instead of silently forwarding the API key.
        self._opener = urlrequest.build_opener(_NoTelemetryRedirect)
        self._thread.start()

    # ── public API ────────────────────────────────────────────────────────

    def stats(self) -> Dict[str, Any]:
        """O11 diagnostics snapshot: delivered / retries / dropped-by-reason
        counters plus the live queue depth. Losses are never silent — every
        drop also notifies ``on_error`` (when set)."""
        with self._lock:
            queued = len(self._buffer)
        return {
            "delivered": self._stats_delivered,
            "retries": self._stats_retries,
            "dropped": dict(self._stats_dropped),
            "queued": queued,
        }

    def _count_loss(self, reason: str, count: int, detail: str = "") -> None:
        """O11: increment the visible counter and notify the hook."""
        self._stats_dropped[reason] = self._stats_dropped.get(reason, 0) + count
        message = f"observe: lost {count} record(s) ({reason})"
        if detail:
            message += f" — {detail}"
        self._last_error = RuntimeError(message)
        if self.on_error is not None:
            try:
                self.on_error(self._last_error)
            except Exception:  # noqa: BLE001 - hook must not break the SDK
                pass

    def _backoff_for(self, attempt: int) -> float:
        delay = float(self.opts.retry_backoff)
        for _ in range(1, attempt):
            delay *= 2
            if delay >= 30.0:
                return 30.0
        return delay

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
            # F12: producer-stable event id - lets the flush-time dedupe on
            # the server drop a redelivered copy instead of double-counting.
            "event_id": uuid.uuid4().hex,
            "producer_id": self._producer_id,
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
        # AUD-036 (round 2): validate serialization AT ADMISSION and queue
        # the OWNED bytes — json.dumps used to run inside the flush loop
        # AFTER the buffer was detached, so one unsupported attribute type
        # raised TypeError out of the loop and silently discarded every
        # valid entry behind it; worse, the original dict stayed mutable by
        # the caller. O11: the rejection is also counted (entry_invalid).
        try:
            raw = encode_entry(entry)
        except ValueError as exc:
            self._count_loss("entry_invalid", 1, str(exc))
            raise
        with self._lock:
            if self._state != "open":
                # AUD-037: no admission once closing starts — a queued
                # entry with no consumer is a silent loss.
                raise RuntimeError("observe_sdk: client is closing or closed")
            if self._buffer_bytes + len(raw) > _MAX_QUEUE_BYTES:
                # O11: drop-newest overflow policy, counted visibly on top
                # of the long-standing BufferError signal.
                self._stats_dropped["queue_full"] = self._stats_dropped.get("queue_full", 0) + 1
                raise BufferError("observe_sdk: log queue byte limit reached")
            self._buffer.append(raw)
            self._buffer_bytes += len(raw)
            full = len(self._buffer) >= self.opts.log_batch_size
        if full:
            self.flush()

    def debug(self, msg: str, **fields: Any) -> None: self.log("debug", msg, **fields)
    def info(self, msg: str, **fields: Any) -> None:  self.log("info", msg, **fields)
    def warn(self, msg: str, **fields: Any) -> None:  self.log("warn", msg, **fields)
    def error(self, msg: str, **fields: Any) -> None: self.log("error", msg, **fields)
    def fatal(self, msg: str, **fields: Any) -> None: self.log("fatal", msg, **fields)

    def flush(self, timeout: Optional[float] = None) -> None:
        """Drain the log buffer synchronously via the batch endpoint.

        AUD-035 (round 2): one flush owner; a batch is removed from the
        queue only once its request succeeded. A failed post leaves the
        entries queued, updates ``last_error``, notifies ``on_error``, and
        re-raises — a caller (or ``close``) can observe the failure instead
        of a silent loss.

        O11 retry/loss contract: retryable failures (429/5xx/network, see
        ``_retryable``) get an immediate first retry; further consecutive
        failures hold off with exponential backoff, and once
        ``max_send_attempts`` is exhausted the chunk is dropped and counted
        (``retry_exhausted``). A non-retryable 4xx is dropped immediately
        (``non_retryable``) — it can never succeed as-shaped, and blocking
        the queue head on it starves every later entry. A 200 whose body
        reports per-entry rejections counts them as ``server_rejected``
        (the accepted neighbors are never resent). ``timeout`` bounds the
        whole drain; on expiry a TimeoutError is raised with the queue
        still intact and visible in ``stats()``. While the backoff gate is
        engaged, flush() returns early — queued entries remain visible via
        ``stats()["queued"]``.
        """
        deadline = time.monotonic() + timeout if timeout is not None else None
        with self._flush_lock:
            with self._lock:
                remaining = len(self._buffer)  # fixed watermark, not a live drain
            while remaining:
                if self._send_attempts >= 2 and time.monotonic() < self._retry_not_before:
                    return  # mid-backoff; the worker or a later flush retries
                if deadline is not None and time.monotonic() >= deadline:
                    raise TimeoutError(
                        "observe_sdk: flush deadline reached with entries queued"
                    )
                with self._lock:
                    chosen: List[bytes] = []
                    size = len(b'{"logs":[]}')
                    for raw in self._buffer[: min(remaining, _MAX_LOG_BATCH)]:
                        extra = len(raw) + (1 if chosen else 0)
                        if size + extra > _MAX_BATCH_BYTES and chosen:
                            break
                        chosen.append(raw)
                        size += extra
                if not chosen:
                    # Belt-and-braces (admission caps entries at 64 KiB, so
                    # this is unreachable in practice): drop the head entry
                    # loudly instead of erroring forever on an undrainable
                    # queue.
                    with self._lock:
                        oversize = self._buffer.pop(0)
                        self._buffer_bytes -= len(oversize)
                    remaining -= 1
                    self._count_loss("entry_oversize", 1, "entry exceeds the request byte budget")
                    continue
                body = b'{"logs":[' + b",".join(chosen) + b']}'
                per_request = self.opts.timeout
                if deadline is not None:
                    per_request = min(per_request, max(deadline - time.monotonic(), 0.05))
                try:
                    accepted, rejected = self._post_batch(body, timeout=per_request)
                except Exception as exc:  # noqa: BLE001 - classified below
                    if not _retryable(exc):
                        with self._lock:
                            del self._buffer[: len(chosen)]
                            self._buffer_bytes -= sum(len(c) for c in chosen)
                        remaining -= len(chosen)
                        self._send_attempts = 0
                        self._count_loss("non_retryable", len(chosen), str(exc))
                        continue
                    self._send_attempts += 1
                    if self._send_attempts >= self.opts.max_send_attempts:
                        with self._lock:
                            del self._buffer[: len(chosen)]
                            self._buffer_bytes -= sum(len(c) for c in chosen)
                        remaining -= len(chosen)
                        self._send_attempts = 0
                        self._count_loss(
                            "retry_exhausted", len(chosen),
                            f"{self.opts.max_send_attempts} attempts, last error: {exc}",
                        )
                        continue
                    self._stats_retries += 1
                    self._retry_not_before = time.monotonic() + self._backoff_for(
                        self._send_attempts
                    )
                    self._last_error = exc
                    if self.on_error is not None:
                        try:
                            self.on_error(exc)
                        except Exception:  # noqa: BLE001 - hook must not break flush
                            pass
                    raise
                self._send_attempts = 0
                if accepted is None and rejected is None:
                    accepted, rejected = len(chosen), 0  # bodyless 200 (old servers)
                if rejected:
                    self._count_loss(
                        "server_rejected", rejected,
                        f"server accepted {accepted} of {len(chosen)}",
                    )
                self._stats_delivered += accepted or 0
                with self._lock:
                    del self._buffer[: len(chosen)]
                    self._buffer_bytes -= sum(len(c) for c in chosen)
                remaining -= len(chosen)
            self._last_error = None

    def close(self, timeout: Optional[float] = None) -> None:
        """Stop the background flusher and drain pending logs.

        AUD-037 (round 2): open -> closing -> closed with an explicit
        failure state. A timed-out join raises TimeoutError (close is
        incomplete, retryable) instead of returning success; a failed drain
        raises and leaves the client in ``closing`` so a later close can
        retry; ``log()`` refuses admission once closing starts.

        O11: ``timeout`` bounds BOTH the worker join and the final drain
        (default: the configured flush interval + request timeout + 1 s).
        Entries still queued when close gives up are counted as
        ``shutdown_unflushed`` losses and reported through ``on_error`` —
        never swallowed.
        """
        with self._close_lock:
            with self._lock:
                if self._state == "closed":
                    return
                self._state = "closing"
            self._stop.set()
            join_timeout = (
                timeout if timeout is not None
                else self.opts.log_flush_interval + self.opts.timeout + 1.0
            )
            self._thread.join(timeout=join_timeout)
            if self._thread.is_alive():
                raise TimeoutError(
                    "observe_sdk: flush worker has not stopped; close is incomplete"
                )
            drain_timeout = timeout if timeout is not None else self.opts.timeout
            drain_error: Optional[BaseException] = None
            try:
                self.flush(timeout=drain_timeout)
            except Exception as exc:  # noqa: BLE001 - surfaced via the raise below
                drain_error = exc
            with self._lock:
                leftover = len(self._buffer)
            if leftover:
                self._count_loss(
                    "shutdown_unflushed", leftover,
                    "close deadline reached with entries queued",
                )
            if drain_error is not None or leftover:
                raise TimeoutError(
                    f"observe_sdk: close incomplete ({leftover} entries queued"
                    f"{f'; last error: {drain_error}' if drain_error else ''})"
                )
            with self._lock:
                self._state = "closed"

    # ── internal ──────────────────────────────────────────────────────────

    def _loop(self) -> None:
        while not self._stop.wait(self.opts.log_flush_interval):
            try:
                self.flush()
            except Exception as exc:  # noqa: BLE001
                # AUD-035: the background loop REPORTS failures instead of
                # `except: pass` — a lost batch was indistinguishable from
                # a successful one at close time.
                self._last_error = exc
                if self.on_error is not None:
                    try:
                        self.on_error(exc)
                    except Exception:  # noqa: BLE001
                        pass

    def _post(self, path: str, body: Dict[str, Any], *, silent: bool = False) -> None:
        url = self.opts.endpoint.rstrip("/") + path
        data = json.dumps(body).encode("utf-8")
        self._post_bytes(path, data, url=url, silent=silent)

    def _post_batch(
        self, body: bytes, *, timeout: Optional[float] = None
    ) -> tuple[Optional[int], Optional[int]]:
        """POST one packed /logs/batch body and return the per-entry ack
        (accepted, rejected) from the response (O11 partial-error
        handling). Raises ObserveHTTPError on non-2xx."""
        url = self.opts.endpoint.rstrip("/") + "/api/v1/logs/batch"
        headers = {"Content-Type": "application/json"}
        if self.opts.api_key:
            headers["X-API-Key"] = self.opts.api_key
        req = urlrequest.Request(url, data=body, headers=headers, method="POST")
        try:
            with self._opener.open(req, timeout=timeout or self.opts.timeout) as resp:
                raw = resp.read(64 * 1024)
        except urlrequest.HTTPError as exc:
            raise ObserveHTTPError("/api/v1/logs/batch", exc.code) from exc
        except (URLError, TimeoutError, OSError) as exc:
            raise RuntimeError(f"observe: post /api/v1/logs/batch failed: {exc}") from exc
        if not 200 <= resp.status < 300:
            raise ObserveHTTPError("/api/v1/logs/batch", resp.status)
        try:
            ack = json.loads(raw.decode("utf-8")) if raw else {}
        except ValueError:
            return None, None
        return (
            ack.get("accepted") if isinstance(ack.get("accepted"), int) else None,
            ack.get("rejected") if isinstance(ack.get("rejected"), int) else None,
        )

    def _post_bytes(self, path: str, data: bytes, *, url: Optional[str] = None, silent: bool = False) -> None:
        if url is None:
            url = self.opts.endpoint.rstrip("/") + path
        headers = {"Content-Type": "application/json"}
        if self.opts.api_key:
            headers["X-API-Key"] = self.opts.api_key
        req = urlrequest.Request(url, data=data, headers=headers, method="POST")
        try:
            with self._opener.open(req, timeout=self.opts.timeout) as resp:
                # TO-037: only 2xx is success (a refused 3xx reaches here
                # as an HTTPError, which is a URLError subclass).
                if not 200 <= resp.status < 300 and not silent:
                    raise ObserveHTTPError(path, resp.status)
        except urlrequest.HTTPError as exc:
            if silent:
                return
            raise ObserveHTTPError(path, exc.code) from exc
        except (URLError, TimeoutError) as exc:
            if not silent:
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
# TO-044: default replacement is serialized and a failed shutdown of the
# outgoing client is PROPAGATED (Client.close deliberately exposes drain
# failures — swallowing them here hid them); one module-level exit handler
# closes whatever client is current at exit instead of retaining every
# client init() ever created.
_default_lock = threading.RLock()


def init(**kwargs: Any) -> Client:
    """Initialize the default client. Repeat calls replace the previous one.

    The replacement's options are validated BEFORE the outgoing client is
    closed, and a failed close of the outgoing client raises (the caller
    can catch it and still complete the swap manually via a second init).
    """
    candidate = Client.__new__(Client)  # validate without starting a worker
    candidate.opts = Options(**kwargs)
    _validate_options(candidate.opts)
    global _default
    with _default_lock:
        previous = _default
        if previous is not None:
            previous.close()  # may raise — the old client stays recoverable
        _default = Client(**kwargs)
        return _default


def _close_default_at_exit() -> None:
    with _default_lock:
        current = _default
    if current is not None:
        try:
            current.close()
        except Exception:  # noqa: BLE001 - interpreter teardown
            pass


atexit.register(_close_default_at_exit)


def _require() -> Client:
    if _default is None:
        raise RuntimeError("observe_sdk: call init() before using the default client")
    return _default


def close() -> None:
    if _default is not None:
        _default.close()


def flush() -> None:
    _require().flush()


def stats() -> Dict[str, Any]:
    """O11 diagnostics of the default client (see Client.stats)."""
    return _require().stats()


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
