# hydn-co/telemetry

Shared OpenTelemetry setup for mesh services: OTLP tracing and log-trace correlation via the slog bridge. Reused across mesh-stream, mesh-core, control, and other projects.

## Configuration

The package requires four environment variables (fail fast: `Setup` panics if any are missing):

- **OTEL_EXPORTER_OTLP_ENDPOINT** — OTLP gRPC endpoint (e.g. `http://127.0.0.1:4317` when using a Datadog agent sidecar).
- **OTEL_SERVICE_NAME** — Service name for the OTel resource and slog correlation (e.g. `mesh-stream`).
- **OTEL_DEPLOYMENT_ENVIRONMENT** — Deployment environment (e.g. `dev1`, `prod`); set on the OTel resource as `deployment.environment`.
- **OTEL_SERVICE_VERSION** — Service version (e.g. `1.0.0`); set on the OTel resource as `service.version`.

Set these in your deployment (e.g. Bicep/Helm) so the process fails fast at startup if misconfigured.

## Usage

```go
import (
    "context"
    "github.com/hydn-co/telemetry"
)

func main() {
    ctx := context.Background()
    shutdown := telemetry.Setup(ctx, telemetry.Options{})
    defer shutdown() // safe to call multiple times
    // ...
}
```

**Log-trace correlation:** The otelslog bridge injects `trace_id` and `span_id` into log records when the **context** passed to the logger contains an active OpenTelemetry span. For correlation to work:

1. **Create spans at entry points** (e.g. HTTP requests) so there is a span in context — use `otelhttp` middleware or `tracer.Start(ctx, ...)` and pass the returned context.
2. **Use context-aware logging** where you have that context: `slog.InfoContext(ctx, ...)`, `slog.ErrorContext(ctx, ...)`. Logs emitted without a context that has a span will not contain trace/span IDs.

Datadog correlates logs and traces when logs have top-level `trace_id` and `span_id` (or equivalent) and the same IDs appear in traces. The required env vars set `service.name`, `deployment.environment`, and `service.version` on the OTel resource.

## Dependency

The module is published and available via the Go proxy:

```bash
go get github.com/hydn-co/telemetry@latest
```

Or pin a specific version (e.g. `@v0.0.1`).

For local development in a multi-repo layout, add to your `go.mod`:

```
replace github.com/hydn-co/telemetry => ../telemetry
```
