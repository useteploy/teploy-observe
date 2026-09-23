# teploy-observe (Python)

Python SDK for [Observe](https://github.com/useteploy/teploy-observe) — self-hosted analytics, errors, logs, traces.

## Install

```
pip install teploy-observe
```

Zero dependencies; stdlib only.

## Usage

```python
import os
import observe_sdk as observe

observe.init(
    endpoint="https://observe.example.com",
    api_key=os.environ["OBSERVE_API_KEY"],
    site_id="default",
    release="v1.4.2",
    service_name="api",
)

try:
    do_work()
except Exception as exc:
    observe.capture_exception(exc)

observe.info("request served", user_id=user_id, duration_ms=elapsed)

# Call on shutdown — also registered via atexit so this is optional.
observe.close()
```

## API

| function | purpose |
|----------|---------|
| `init(...)` | Create and register the module-level client. |
| `capture_exception(exc)` | Submit an error with stack trace. |
| `log(level, msg, **fields)` | Emit a log. |
| `debug / info / warn / error / fatal` | Level helpers. |
| `flush(timeout=None)` | Drain buffered logs synchronously under an optional whole-drain deadline. |
| `close(timeout=None)` | Stop the flush thread and drain; leftovers at the deadline are counted as `shutdown_unflushed` losses. |
| `stats()` | O11 diagnostics: delivered / retries / dropped-by-reason counters + live queue depth. |

## Guarantees

- **Non-blocking logs.** Entries buffer in memory and flush on a timer or when the batch fills.
- **Immediate errors.** Exceptions ship before a crash can lose them.
- **Stack traces.** Frames under `site-packages` / stdlib are marked `in_app: false`.
- **No external deps.** Uses `urllib` + `threading` — fine in Lambda, Docker slim, etc.

## Delivery, loss, and shutdown semantics (O11)

- **Bounded queue** (8 MiB queued bytes; per-entry 64 KiB at admission).
  Overflow raises `BufferError` (drop-newest policy) AND counts a visible
  `queue_full` loss in `stats()`.
- **Bounded retry with backoff**: retryable failures (429/5xx/network) get
  an immediate first retry, then exponential backoff
  (`retry_backoff`, default 1 s, 30 s cap) up to `max_send_attempts`
  (default 6) before the chunk is dropped and counted
  (`retry_exhausted`). A non-retryable 4xx drops its chunk immediately
  (`non_retryable`) — it can never succeed as-shaped.
- **Partial errors**: the batch ack's per-entry `rejected` count becomes a
  `server_rejected` loss; the accepted neighbors are never resent.
- **Shutdown**: `close(timeout=...)` bounds worker join and drain;
  leftovers are counted as `shutdown_unflushed` and reported via
  `on_error`. Signal recipe:

```python
import signal, observe_sdk

def _shutdown(signum, frame):
    try:
        observe_sdk.close(timeout=5.0)
    except TimeoutError as exc:
        print(f"observe losses: {observe_sdk.stats()}", file=sys.stderr)
    raise SystemExit(0)

signal.signal(signal.SIGTERM, _shutdown)
signal.signal(signal.SIGINT, _shutdown)
```

- Queues are in-memory; `kill -9` loses the queue and its counters with
  the process. No durable local spool is claimed.

## License

MIT
