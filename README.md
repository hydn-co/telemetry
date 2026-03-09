# hydn-co/telemetry

Shared OpenTelemetry **tracing** (OTLP) and structured **JSON slog** logging with Datadog-friendly correlation. Used by mesh-stream, mesh-core, control, and other projects.

This package does **not** use or require OpenTelemetry logs export. Primary logging is standard `log/slog` JSON to stdout (and optionally to a file). The Datadog agent can collect via stdout or by tailing the log file.

## Configuration

Required environment variables (Setup panics if any are missing):

- **OTEL_EXPORTER_OTLP_ENDPOINT** â€” OTLP gRPC endpoint (e.g. `http://127.0.0.1:4317` for a Datadog agent sidecar).
- **OTEL_SERVICE_NAME** â€” Service name (e.g. `mesh-stream`, `mesh-core-portal`). Set on the OTel resource and on every log record as `service`.
- **OTEL_DEPLOYMENT_ENVIRONMENT** â€” Deployment environment (e.g. `dev1`, `prod`). Set on the OTel resource and on every log record as `env`.
- **OTEL_SERVICE_VERSION** â€” Service version (e.g. `1.0.0`). Set on the OTel resource and on every log record as `version`.

Optional:

- **LOG_FILE** â€” When set, logs are also written to this path (JSON, one record per line). Used with a shared volume so a Datadog agent sidecar can tail the file (e.g. `/LogFiles/app.log`). The file is opened append-only and closed on shutdown. If opening fails, an error is logged and the process continues without file logging.

Set the required variables in your deployment (e.g. Bicep/Helm) so the process fails fast at startup if misconfigured.

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

## Logging and correlation

**Primary logging:** Setup installs a default `slog` logger that writes **JSON** to stdout (and to `LOG_FILE` if set). Each log record includes top-level fields required for Datadog unified service tagging and logâ€“trace correlation:

| Field       | When present | Format / meaning |
|------------|--------------|-------------------|
| `service`  | Always       | From `OTEL_SERVICE_NAME`. |
| `env`      | Always       | From `OTEL_DEPLOYMENT_ENVIRONMENT`. |
| `version`  | Always       | From `OTEL_SERVICE_VERSION`. |
| `trace_id` | When the log context has an active span | 32-character lowercase hex. |
| `span_id`  | When the log context has an active span | 16-character lowercase hex. |

So logs are self-describing: no need to rely on the sidecar to infer service, env, or version. Datadog correlates logs and traces when `trace_id` and `span_id` are present and match trace IDs.

**To get trace/span IDs on logs:** Use context-aware logging where you have a request or span context: `slog.InfoContext(ctx, ...)`, `slog.ErrorContext(ctx, ...)`. Create spans at entry points (e.g. HTTP handlers via `otelhttp` or `tracer.Start(ctx, ...)`) and pass the context through. Logs emitted without a context that contains a span will still have `service`, `env`, and `version` but not `trace_id`/`span_id`.

## Tracing

Setup configures an OTLP gRPC trace exporter and sets the global tracer provider. Tracing is best-effort: if the exporter or resource setup fails, Setup still installs the logger and returns a shutdown function that closes the log file (if any). The process keeps running with logging and without tracing.

## File logging for sidecar collection

In container environments (e.g. Azure Container Apps), set `LOG_FILE=/LogFiles/app.log` and mount a shared volume at `/LogFiles` for both the app and a Datadog agent sidecar. The sidecar tails the file; each line is a JSON object with `service`, `env`, `version`, and optionally `trace_id` and `span_id`.

### Logs not showing in Datadog

**Likely cause:** `DD_SERVERLESS_LOG_PATH` (file tailing from a shared volume) is supported by the **serverless-init** sidecar image, not by the full **agent** image. If you use `datadoghq.azurecr.io/agent:7` (or `datadog/agent`) and write logs only to `/LogFiles/app.log`, the full agent handles OTLP traces but does **not** tail that file.

**Recommended Azure Container Apps setup:**

1. Keep the full Datadog **agent** sidecar for OTLP traces (`127.0.0.1:4317` / `4318`).
2. Add a separate **serverless-init** sidecar for log tailing from the shared volume.
3. Keep the app writing JSON logs to `LOG_FILE=/LogFiles/app.log`.

With that split:

- **Tracing sidecar:** `datadoghq.azurecr.io/agent:7` with OTLP receiver env vars.
- **Logging sidecar:** `docker.io/datadog/serverless-init:1` with `DD_SERVERLESS_LOG_PATH=/LogFiles/app.log`, `DD_LOGS_ENABLED=true`, and the same `DD_SERVICE` / `DD_ENV` / `DD_VERSION` tags.

**Verification:**

- In the **app** container: confirm `/LogFiles/app.log` exists and contains the line with `"msg":"telemetry initialized"` (written right after Setup). If that line is missing, telemetry did not run or `LOG_FILE` was not set.
- In the **logging sidecar**: confirm it is `serverless-init` and has the same `/LogFiles` mount plus `DD_SERVERLESS_LOG_PATH=/LogFiles/app.log`.
- In **Datadog**: filter by your `service` and `env`; check Logs -> Search and pipelines so logs are not dropped or mis-tagged.

## Dependency
```bash
go get github.com/hydn-co/telemetry@latest
```

Or pin a version (e.g. `@v0.0.1`). For local development in a multi-repo layout:

```
replace github.com/hydn-co/telemetry => ../telemetry
```
