"""Breadcrumbs: a bounded record of what happened before an error.

The wire shape matches the server's error ingest (internal/errors
Breadcrumb): ``type``, ``category``, ``message``, ``data``, ``timestamp``
(Unix epoch milliseconds) and ``level``.

The buffer is process-wide and thread-safe. It is a ring: once full, the
oldest entry is dropped. It is not request-scoped, so on a server handling
concurrent requests the breadcrumbs attached to an error are the most
recent activity of the whole process, not of one request.
"""

from __future__ import annotations

import collections
import json
import logging
import threading
import time
from typing import Any, Callable, Deque, Dict, List, Optional

DEFAULT_MAX_BREADCRUMBS = 100
MAX_MESSAGE = 256
MAX_DATA_BYTES = 1024
# Budget for the breadcrumbs attached to one payload; oldest are dropped first.
MAX_ATTACH_BYTES = 32 * 1024

_LEVELS = ("debug", "info", "warning", "error")

BeforeBreadcrumb = Callable[[Dict[str, Any]], Optional[Dict[str, Any]]]


def _clone_data(data: Any) -> Optional[Dict[str, Any]]:
    """Own-data snapshot via a JSON round trip, size-capped."""
    if data is None:
        return None
    try:
        raw = json.dumps(data, default=str, allow_nan=False)
    except (TypeError, ValueError):
        return {"_unserializable": True}
    if len(raw) > MAX_DATA_BYTES:
        return {"_truncated": True, "_bytes": len(raw)}
    out = json.loads(raw)
    return out if isinstance(out, dict) else {"value": out}


def _level(value: Any, default: str = "info") -> str:
    return value if isinstance(value, str) and value in _LEVELS else default


class BreadcrumbBuffer:
    """Thread-safe bounded ring of breadcrumbs."""

    def __init__(
        self,
        max_size: int = DEFAULT_MAX_BREADCRUMBS,
        before_breadcrumb: Optional[BeforeBreadcrumb] = None,
        on_error: Optional[Callable[[BaseException], None]] = None,
    ) -> None:
        self._lock = threading.Lock()
        self._items: Deque[Dict[str, Any]] = collections.deque(maxlen=max_size)
        self._before = before_breadcrumb
        self._on_error = on_error

    def add(
        self,
        message: str,
        *,
        category: str = "default",
        type: str = "default",  # noqa: A002 - wire field name
        level: str = "info",
        data: Optional[Dict[str, Any]] = None,
        timestamp: Optional[int] = None,
    ) -> None:
        """Record a breadcrumb. Never raises."""
        try:
            crumb: Dict[str, Any] = {
                "type": str(type)[:64],
                "category": str(category)[:64],
                "message": str(message)[:MAX_MESSAGE],
                "timestamp": int(timestamp) if isinstance(timestamp, (int, float)) and not isinstance(timestamp, bool)
                else int(time.time() * 1000),
                "level": _level(level),
            }
            cloned = _clone_data(data)
            if cloned is not None:
                crumb["data"] = cloned
            if self._before is not None:
                try:
                    edited = self._before(crumb)
                except Exception as exc:  # noqa: BLE001
                    # Fail closed: the hook is usually a scrubber.
                    self._report(exc)
                    return
                if not edited:
                    return
                crumb = {
                    "type": str(edited.get("type", crumb["type"]))[:64],
                    "category": str(edited.get("category", crumb["category"]))[:64],
                    "message": str(edited.get("message", ""))[:MAX_MESSAGE],
                    "timestamp": edited.get("timestamp", crumb["timestamp"])
                    if isinstance(edited.get("timestamp"), int) else crumb["timestamp"],
                    "level": _level(edited.get("level"), crumb["level"]),
                }
                cloned = _clone_data(edited.get("data"))
                if cloned is not None:
                    crumb["data"] = cloned
            with self._lock:
                self._items.append(crumb)
        except Exception:  # noqa: BLE001 - recording must never break the caller
            pass

    def _report(self, exc: BaseException) -> None:
        if self._on_error is not None:
            try:
                self._on_error(exc)
            except Exception:  # noqa: BLE001
                pass

    def snapshot(self) -> List[Dict[str, Any]]:
        """Oldest-first copy, bounded to the attach byte budget."""
        with self._lock:
            items = list(self._items)
        out: List[Dict[str, Any]] = []
        used = 2
        for crumb in reversed(items):
            size = len(json.dumps(crumb)) + 1
            if used + size > MAX_ATTACH_BYTES:
                break
            used += size
            copy = dict(crumb)
            if "data" in copy:
                copy["data"] = json.loads(json.dumps(copy["data"], allow_nan=False))
            out.append(copy)
        out.reverse()
        return out

    def clear(self) -> None:
        with self._lock:
            self._items.clear()

    def __len__(self) -> int:
        with self._lock:
            return len(self._items)


_PY_LEVELS = (
    (logging.ERROR, "error"),
    (logging.WARNING, "warning"),
    (logging.INFO, "info"),
)


class BreadcrumbHandler(logging.Handler):
    """``logging`` handler that records records as breadcrumbs.

    Records from the SDK's own logger (``observe_sdk``) are skipped so the
    SDK cannot record its own diagnostics, and a thread-local guard stops
    recursion if a ``before_breadcrumb`` hook logs.
    """

    def __init__(self, buffer: BreadcrumbBuffer, level: int = logging.WARNING) -> None:
        super().__init__(level=level)
        self._buffer = buffer
        self._guard = threading.local()

    def emit(self, record: logging.LogRecord) -> None:
        try:
            if record.name == "observe_sdk" or record.name.startswith("observe_sdk."):
                return
            if getattr(self._guard, "active", False):
                return
            self._guard.active = True
            try:
                level = "debug"
                for threshold, name in _PY_LEVELS:
                    if record.levelno >= threshold:
                        level = name
                        break
                self._buffer.add(
                    record.getMessage(),
                    type="log",
                    category=record.name,
                    level=level,
                    data={"logger": record.name, "level_name": record.levelname},
                    timestamp=int(record.created * 1000),
                )
            finally:
                self._guard.active = False
        except Exception:  # noqa: BLE001 - logging handlers must not raise
            pass
