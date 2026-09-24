# observe (Go)

Go SDK for [Observe](https://github.com/useteploy/teploy-observe) — self-hosted analytics, errors, logs, traces.

## Install

```
go get github.com/useteploy/teploy-observe/sdk/go
```

## Usage

```go
import observe "github.com/useteploy/teploy-observe/sdk/go"

func main() {
    client, err := observe.New(observe.Options{
        Endpoint:    "https://observe.example.com",
        APIKey:      os.Getenv("OBSERVE_API_KEY"),
        SiteID:      "default",
        Release:     "v1.4.2",
        ServiceName: "api",
    })
    if err != nil { log.Fatal(err) }
    defer client.Close()

    if err := doWork(); err != nil {
        client.CaptureException(err)
    }

    client.Info("request served",
        observe.F("user_id", userID),
        observe.F("duration_ms", elapsed.Milliseconds()),
    )
}
```

## Guarantees

- **Non-blocking logs.** `Info/Warn/Error` buffer in memory and flush on a timer or when the batch fills. `Close()`/`Shutdown(ctx)` drain the buffer.
- **Immediate errors.** `CaptureException` ships right away — no risk of losing the last error before a crash.
- **Captured stack traces.** Frames from stdlib and GOROOT are marked `in_app: false` so the UI can highlight your code.
- **Zero dependencies.** Just the standard library.

## Delivery, loss, and shutdown semantics (O11)

- **Bounded queues.** Logs, spans, and metric series/points are capped in
  count and bytes. Admission overflow drops the NEW record (documented
  drop-newest policy) and counts it.
- **Bounded retry with backoff.** Retryable failures (429/5xx/network)
  retry: the first retry is immediate, further consecutive failures back
  off exponentially (1 s base, 30 s cap) up to `MaxSendAttempts` (default
  6), after which the chunk is dropped and counted. A non-retryable 4xx is
  dropped immediately — it can never succeed as-shaped, and blocking the
  queue head on it (the old behavior) silently starved everything behind.
- **Partial errors.** `/logs/batch` acknowledgments carry per-entry
  outcomes; `rejected` entries are counted as `logs_server_rejected`
  losses and the accepted neighbors are never resent.
- **Visible loss counters.** `client.Stats()` returns delivered/retry/loss
  counters by reason (see the `Stats` godoc). Every loss also reports
  through `OnError`; at shutdown a one-line loss summary is emitted when
  anything was lost.
- **Shutdown deadlines.** `Shutdown(ctx)` carries YOUR deadline for the
  final drain; leftovers when it expires are counted per signal as
  `<signal>_shutdown_unflushed` and reported, never swallowed. Signal-path
  recipe:

```go
sig := make(chan os.Signal, 1)
signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
<-sig
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()
if err := client.Shutdown(ctx); err != nil {
    log.Printf("observe shutdown: %v (losses: %+v)", err, client.Stats().Dropped)
}
```

- Queues are in-memory; `kill -9` loses the queue and its counters with
  the process. No durable local spool is claimed.

## License

MIT
