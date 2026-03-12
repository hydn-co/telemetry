# hydn-co/telemetry

Shared Azure Container Apps-specific OpenTelemetry bootstrap for localhost OTLP **tracing**, **metrics** export, OpenTelemetry **logs** export, and structured **JSON slog** logging with Datadog-friendly correlation. Used by mesh-stream, mesh-core, control, and other projects.

Primary logging remains standard `log/slog` JSON to stdout (and optionally to a file), but `Setup` now also bridges `slog` records into the OpenTelemetry logs signal and installs a global OTLP meter provider. This package is intentionally scoped to Azure Container Apps managed OTLP with a localhost endpoint, not sidecar-based collectors.

## Configuration

Required environment variables (Setup panics if any are missing):

- **OTEL_EXPORTER_OTLP_ENDPOINT** â€” OTLP gRPC endpoint for traces, logs, and metrics (e.g. `http://127.0.0.1:4317` for a local sidecar/agent, or the ACA-injected endpoint in Azure Container Apps).
- **OTEL_SERVICE_NAME** â€” Service name (e.g. `mesh-stream`, `mesh-core-portal`). Set on the OTel resource and on every log record as `service`.
- **OTEL_DEPLOYMENT_ENVIRONMENT** â€” Deployment environment (e.g. `dev1`, `prod`). Set on the OTel resource and on every log record as `env`.
- **OTEL_SERVICE_VERSION** â€” Service version (e.g. `1.0.0`). Set on the OTel resource and on every log record as `version`.

Optional:

- **LOG_FILE** â€” When set, logs are also written to this path (JSON, one record per line). This is optional and useful when a separate collector tails a file (for example, `/LogFiles/app.log`). The file is opened append-only and closed on shutdown. If opening fails, an error is logged and the process continues without file logging.

Set the required variables in your deployment so the process fails fast at startup if misconfigured.

## Usage

```go
import (
    "context"
    "github.com/hydn-co/telemetry"
)

func main() {
    ctx := context.Background()
    shutdown := telemetry.Setup(ctx, telemetry.Options{})
    defer shutdown() // idempotent
    // ...
}
```

## Logging and correlation

**Primary logging:** Setup installs a default `slog` logger that writes **JSON** to stdout (and to `LOG_FILE` if set). Each log record includes top-level fields required for Datadog unified service tagging and log-trace correlation:

| Field       | When present | Format / meaning |
|------------|--------------|-------------------|
| `service`  | Always       | From `OTEL_SERVICE_NAME`. |
| `env`      | Always       | From `OTEL_DEPLOYMENT_ENVIRONMENT`. |
| `version`  | Always       | From `OTEL_SERVICE_VERSION`. |
| `trace_id` | When the log context has an active span | 32-character lowercase hex. |
| `span_id`  | When the log context has an active span | 16-character lowercase hex. |

So logs are self-describing: Datadog does not need to infer service, env, or version from container metadata alone. Datadog correlates logs and traces when `trace_id` and `span_id` are present and match trace IDs.

**To get trace/span IDs on logs:** Use context-aware logging where you have a request or span context: `slog.InfoContext(ctx, ...)`, `slog.ErrorContext(ctx, ...)`. Create spans at entry points (e.g. HTTP handlers via `otelhttp` or `tracer.Start(ctx, ...)`) and pass the context through. Logs emitted without a context that contains a span will still have `service`, `env`, and `version` but not `trace_id`/`span_id`.

## OpenTelemetry logs export

`Setup` also creates an OTLP gRPC log exporter and bridges the default `slog` logger into the OpenTelemetry logs signal. That means a single `slog.Info(...)` call does three things:

1. Writes JSON to stdout.
2. Writes JSON to `LOG_FILE` when configured.
3. Exports the same record through OTLP logs when the exporter is available.

This is best-effort. If OTLP log export cannot be initialized, stdout/file logging still works and the process continues.

## Tracing

Setup configures an OTLP gRPC trace exporter and sets the global tracer provider. Tracing is also best-effort: if the trace exporter or resource setup fails, Setup still installs the logger and returns a shutdown function that closes the log file (if any). The process keeps running with logging and without tracing.

## Metrics

`Setup` also creates an OTLP gRPC metric exporter and installs a global OpenTelemetry `MeterProvider` backed by a periodic reader. This makes the metrics pipeline available to application code via `otel.Meter(...)`.

This only enables export. Services still need to create instruments such as counters, histograms, or observable gauges before Datadog will show application metrics.

## Azure Container Apps without sidecars

For Azure Container Apps with environment-level managed OpenTelemetry:

- Keep `MESH_STREAM_TELEMETRY_ENABLED=true` (or the equivalent service toggle).
- Set `OTEL_SERVICE_NAME`, `OTEL_DEPLOYMENT_ENVIRONMENT`, and `OTEL_SERVICE_VERSION`.
- Let ACA inject `OTEL_EXPORTER_OTLP_ENDPOINT`.
- Do not rely on `stdout` scraping alone for Datadog. The OTLP log export from this package is what allows ACA-managed forwarding to send application logs to Datadog.

With this setup, the same app emits:

- local/Azure-visible JSON logs on stdout
- OTLP traces
- OTLP metrics from any registered instruments
- OTLP logs for Datadog forwarding

## File logging for alternate collectors

If you still need a file-based collector, set `LOG_FILE=/LogFiles/app.log` and mount a shared volume at `/LogFiles`. Each line is a JSON object with `service`, `env`, `version`, and optionally `trace_id` and `span_id`.

### Logs not showing in Datadog

**Likely cause:** the app is only writing logs to stdout or a tailed file, but the deployed environment is only forwarding OTLP telemetry. In that case traces can appear while logs do not.

**Recommended no-sidecar Azure Container Apps setup:**

1. Emit application logs through `slog`.
2. Let this package bridge them to OTLP logs.
3. Let ACA managed OpenTelemetry forward both traces and logs to Datadog.

**Sidecar/file-tail alternative:**

- Keep a Datadog agent or `serverless-init` collector that can actually read stdout or tail `LOG_FILE`.
- Use `LOG_FILE=/LogFiles/app.log` only when that collector path is present.

**Verification:**

- In the **app** logs: confirm the `"msg":"telemetry initialized"` record includes `"otlp_logs_enabled":true` and `"otlp_metrics_enabled":true`.
- In the ACA environment: confirm `logsConfiguration.destinations` includes `dataDog`.
- In the ACA environment: confirm `metricsConfiguration.destinations` includes `dataDog`.
- In **Datadog**: search by `service` and `env`, then inspect raw attributes to confirm the logs are arriving as OTLP-backed application logs.

## Dependency
```bash
go get github.com/hydn-co/telemetry@latest
```

Or pin a version (e.g. `@v0.0.1`). For local development in a multi-repo layout:

```
replace github.com/hydn-co/telemetry => ../telemetry
```
