# hydn-co/telemetry

Shared OpenTelemetry bootstrap for Go services that export **traces**, **metrics**, and **logs** over **OTLP/gRPC** (insecure) to a local endpoint—primarily **Azure Container Apps** managed OpenTelemetry forwarding to **Datadog**.

It also installs a correlated **`log/slog`** pipeline: JSON to stdout or stderr, optional file copy, OTLP log export, and fields suited to Datadog log–trace correlation.

**Callers** should only invoke `telemetry.Setup` when their app is configured for OTLP (for example after checking a service-specific `*_TELEMETRY_ENABLED` env var). This package does not read those flags.

---

## What `Setup` does

1. **Validates** required `OTEL_*` env vars (panics if any are missing).
2. Builds an OTLP **resource** (service, deployment, optional K8s/ACA attributes). The resource **never** sets `datadog.host.name` (and strips it from `OTEL_RESOURCE_ATTRIBUTES` if present).
3. Installs **slog**: primary JSON sink (stdout by default, or `Options.PrimaryLogWriter`), optional `LOG_FILE`, correlation fields, OTLP log bridge.
4. Registers global **MeterProvider** (OTLP metrics) and **TracerProvider** (OTLP traces).
5. Returns an **idempotent** shutdown function—`defer` it on exit.

Export failures for logs/metrics/traces are logged; the process keeps running when possible. Trace exporter creation failure returns a shutdown that still flushes logs/metrics.

---

## Required environment variables

`Setup` **panics** if any of these are empty:

| Variable | Purpose |
|----------|---------|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP gRPC endpoint (e.g. ACA-injected `127.0.0.1:4317`). |
| `OTEL_SERVICE_NAME` | `service.name` and log field `service`. |
| `OTEL_DEPLOYMENT_ENVIRONMENT` | Deployment env (resource + log field `env`). |
| `OTEL_SERVICE_VERSION` | `service.version` and log field `version`. |

---

## Optional environment variables

| Variable | Purpose |
|----------|---------|
| `LOG_LEVEL` | Minimum level for the primary JSON handler and OTLP log bridge (default `info`). Values: `slog` text levels (`debug`, `info`, …). |
| `LOG_FILE` | Append JSON logs to this path (debug level in file). Closed on shutdown. |
| `OTEL_RESOURCE_ATTRIBUTES` | Comma-separated `key=value` pairs merged into the OTLP resource. Entries with key `datadog.host.name` are **ignored** so nothing in-process sets that attribute. |

### Optional resource hints (read if set)

Used for OTLP resource enrichment only; see `setup.go` (`resourceAttrsFromEnv`):

`OTEL_SERVICE_NAMESPACE`, `POD_NAME`, `POD_NAMESPACE`, `POD_UID`, `NODE_NAME`, `CONTAINER_NAME`, `CONTAINER_APP_NAME`, `CONTAINER_APP_REPLICA_NAME`, `CONTAINER_APP_REVISION`, `AWS_REGION`, `AZURE_REGION`, `GOOGLE_CLOUD_REGION`.

---

## Datadog: services, logs, and APM hosts

- **Unified tagging:** Resource uses OpenTelemetry semantic keys Datadog maps to `service`, `env`, and `version` ([semantic mapping](https://docs.datadoghq.com/opentelemetry/mapping/semantic_mapping/)). Each log line also includes `service`, `service.name`, `env`, and `version`, plus `trace_id` / `span_id` when the log context carries an active span.
- **APM / infra host:** This package does **not** set OTLP resource **`datadog.host.name`**. Datadog derives host from its agent, cloud metadata, or other ingest rules ([hostname mapping](https://docs.datadoghq.com/opentelemetry/mapping/hostname/)).
- **Deployment:** Many teams also set **`DD_SERVICE`**, **`DD_ENV`**, **`DD_VERSION`** in Container Apps / Kubernetes for ingest paths that read them; this package does not require them but they align with Datadog docs.

---

## `telemetry.Options`

```go
type Options struct {
    // Primary JSON slog output. Default: os.Stdout. Use os.Stderr when stdout must stay
    // reserved (e.g. MCP JSON-RPC on stdout).
    PrimaryLogWriter io.Writer
}
```

---

## Usage

```go
import (
    "context"
    "github.com/hydn-co/telemetry"
)

func main() {
    ctx := context.Background()
    shutdown := telemetry.Setup(ctx, telemetry.Options{})
    defer shutdown()
    // ...
}
```

Use **`slog.InfoContext` / `slog.ErrorContext`** (and an instrumented HTTP stack or manual spans) so logs include `trace_id` and `span_id` when a span is active.

---

## Verification

After startup, at **debug** level, look for `telemetry initialized` with `otlp_logs_enabled` and `otlp_metrics_enabled`. Ensure your ACA environment sends **logs**, **traces**, and **metrics** to Datadog when you expect all three.

---

## Dependency

```bash
go get github.com/hydn-co/telemetry@latest
```

Local multi-repo development:

```text
replace github.com/hydn-co/telemetry => ../telemetry
```
