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
| `close(timeout=None)` | Stop the flush thread and drain; incomplete close raises and leaves records queued for a later close. |
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
  one deadline covers ownership waits, worker join, and drain. An incomplete
  close raises `TimeoutError`; retained records stay in `stats()["queued"]`
  and a later close can deliver them. Signal recipe:

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

## Breadcrumbs

```python
observe.init(endpoint=..., api_key=..., max_breadcrumbs=100,
             logging_breadcrumbs=True,            # opt-in: logging >= WARNING
             logging_breadcrumb_level=logging.WARNING,
             before_breadcrumb=lambda c: None if c["category"] == "noisy" else c)
observe.add_breadcrumb("cache miss", category="db", level="warning", data={"key": "u:1"})
observe.capture_message("slow job", level="warning")
```

The buffer is a thread-safe, process-wide ring (not request-scoped) attached
to `capture_exception` / `capture_message`. `logging_breadcrumbs` installs one
handler on the root logger (removed by `close()`); `client.install_logging_breadcrumbs(level, logger)`
targets another logger. Records from `observe_sdk` itself are skipped. A
`before_breadcrumb` that raises drops the breadcrumb.

## Feature flags

```python
r = observe.evaluate_flag("new-checkout", user_id="u1",
                          attributes={"plan": "pro"}, default={"enabled": False})
# {"key", "enabled", "variant"?, "reason", "source": "server" | "default", "error"?}
```

`POST /api/v1/flags/evaluate` over the client's transport (no redirects, API
key header). Default timeout is min(client timeout, 3 s). Never raises:
failures, timeouts and server fail-safe answers return `default` with
`source == "default"`. No cache, no exposure event. See
`docs/sdk/BREADCRUMBS_FLAGS.md`.

## License

MIT
