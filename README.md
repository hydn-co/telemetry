# hydn-co/telemetry

Shared OpenTelemetry setup for mesh services: OTLP tracing and log-trace correlation via the slog bridge. Reused across mesh-stream, mesh-core, control, and other projects.

## Configuration

The package reads standard environment variables (no project-specific config):

- **OTEL_EXPORTER_OTLP_ENDPOINT** — OTLP gRPC endpoint (e.g. `http://127.0.0.1:4317` when using a Datadog agent sidecar). When set (together with or without OTEL_SERVICE_NAME), tracing is enabled.
- **OTEL_SERVICE_NAME** — Service name for the OTel resource and slog correlation. Can also be passed in `Options.ServiceName`.

When neither is set, `Setup` returns a no-op shutdown and no tracer is started.

## Usage

```go
import (
    "context"
    "github.com/hydn-co/telemetry"
)

func main() {
    ctx := context.Background()
    shutdown := telemetry.Setup(ctx, telemetry.Options{
        ServiceName:  "mesh-stream", // or your service name
        Version:      version,
        Environment:  "prod",       // optional; sets deployment.environment on the OTel resource
    })
    defer shutdown() // safe to call multiple times
    // ...
}
```

Use `slog.InfoContext(ctx, ...)` (and other context-aware logging) so the slog bridge can attach trace/span IDs for correlation in Datadog (or any OTLP backend).

## Dependency

```bash
go get github.com/hydn-co/telemetry@v0.0.0
```

For local development in a multi-repo layout, add to your `go.mod`:

```
replace github.com/hydn-co/telemetry => ../telemetry
```
