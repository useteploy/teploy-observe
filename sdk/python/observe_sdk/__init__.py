"""Observe Python SDK — self-hosted analytics, errors, logs, traces.

Quick start:

    import observe_sdk as observe

    observe.init(
        endpoint="https://observe.example.com",
        api_key=os.environ["OBSERVE_API_KEY"],
        site_id="default",
        release="v1.4.2",
    )

    try:
        do_work()
    except Exception as exc:
        observe.capture_exception(exc)

    observe.info("request served", user_id=user_id, duration_ms=elapsed)

    observe.close()  # flushes buffered logs
"""

from .client import (
    Client,
    init,
    close,
    flush,
    capture_exception,
    log,
    debug,
    info,
    warn,
    error,
    fatal,
)

__all__ = [
    "Client",
    "init",
    "close",
    "flush",
    "capture_exception",
    "log",
    "debug",
    "info",
    "warn",
    "error",
    "fatal",
]

__version__ = "0.1.0"
