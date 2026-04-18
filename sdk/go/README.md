# observe (Go)

Go SDK for [Observe](https://observe.dev) — self-hosted analytics, errors, logs, traces.

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

- **Non-blocking logs.** `Info/Warn/Error` buffer in memory and flush on a timer or when the batch fills. `Close()` drains the buffer.
- **Immediate errors.** `CaptureException` ships right away — no risk of losing the last error before a crash.
- **Captured stack traces.** Frames from stdlib and GOROOT are marked `in_app: false` so the UI can highlight your code.
- **Zero dependencies.** Just the standard library.

## License

MIT
