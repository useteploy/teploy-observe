# observe-sdk (Python)

Python SDK for [Observe](https://observe.dev) — self-hosted analytics, errors, logs, traces.

## Install

```
pip install observe-sdk
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
| `flush()` | Drain buffered logs synchronously. |
| `close()` | Stop the flush thread and drain. |

## Guarantees

- **Non-blocking logs.** Entries buffer in memory and flush on a timer or when the batch fills.
- **Immediate errors.** Exceptions ship before a crash can lose them.
- **Stack traces.** Frames under `site-packages` / stdlib are marked `in_app: false`.
- **No external deps.** Uses `urllib` + `threading` — fine in Lambda, Docker slim, etc.

## License

MIT
