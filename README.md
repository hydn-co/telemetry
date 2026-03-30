# hydn-co/telemetry

Shared OpenTelemetry bootstrap for Go services that export **traces**, **metrics**, and **logs** over **OTLP/gRPC** (insecure) to a local endpoint—primarily **Azure Container Apps** managed OpenTelemetry forwarding to **Datadog**.

It also installs a correlated **`log/slog`** pipeline: JSON to stdout or stderr, optional file copy, OTLP log export, and fields suited to Datadog log–trace correlation.

**Callers** should only invoke `telemetry.Setup` when their app is configured for OTLP (for example after checking a service-specific `*_TELEMETRY_ENABLED` env var). This package does not read those flags.

---

## What `Setup` does

1. **Validates** required `OTEL_*` env vars (panics if any are missing).
2. Builds an OTLP **resource** (service, deployment, process, OS, optional K8s/ACA attributes). The resource **never** sets **host** semantic attributes (`host`, `host.*`, `datadog.host.name`); those keys are stripped from `OTEL_RESOURCE_ATTRIBUTES` if present.
3. Installs **slog**: primary JSON sink (stdout by default, or `Options.PrimaryLogWriter`), optional `LOG_FILE`, correlation fields (including top-level **`service`** for Datadog), OTLP log bridge (`otelslog` scope name + version from `OTEL_SERVICE_*`).
4. Registers global **MeterProvider** (OTLP metrics) and **TracerProvider** (OTLP traces).
5. Returns an **idempotent** shutdown function—`defer` it on exit.

Export failures for logs/metrics/traces are logged; the process keeps running when possible. Trace exporter creation failure returns a shutdown that still flushes logs/metrics.

---

## Required environment variables

`Setup` **panics** if any of these are empty:

| Variable | Purpose |
|----------|---------|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP gRPC endpoint (e.g. ACA-injected `127.0.0.1:4317`). |
| `OTEL_SERVICE_NAME` | Resource `service.name`, OTLP log bridge **scope name**, and per-line flat **`service`** (Datadog Service facet). `service.*` is not duplicated on each log line. |
| `OTEL_DEPLOYMENT_ENVIRONMENT` | Deployment env (resource + log field `env`). |
| `OTEL_SERVICE_VERSION` | Resource `service.version`, OTLP log bridge **scope version**, and log field `version`. |

---

## Optional environment variables

| Variable | Purpose |
|----------|---------|
| `LOG_LEVEL` | Minimum level for the primary JSON handler and OTLP log bridge (default `info`). Values: `slog` text levels (`debug`, `info`, …). |
| `LOG_FILE` | Append JSON logs to this path (debug level in file). Closed on shutdown. |
| `OTEL_RESOURCE_ATTRIBUTES` | Comma-separated `key=value` pairs merged into the OTLP resource. Keys **`host`**, **`host.*`**, **`hostname`**, **`datadog.host.name`**, bare **`service`**, and **`service.name`** are sanitized out so this package stays authoritative for Datadog host and service identity. |

### Optional resource hints (read if set)

Used for OTLP resource enrichment only; see `setup.go` (`resourceAttrsFromEnv`):

`OTEL_SERVICE_NAMESPACE`, `POD_NAME`, `POD_NAMESPACE`, `POD_UID`, `NODE_NAME`, `CONTAINER_NAME`, `CONTAINER_APP_NAME`, `CONTAINER_APP_REPLICA_NAME`, `AWS_REGION`, `AZURE_REGION`, `GOOGLE_CLOUD_REGION`. (Azure also sets `CONTAINER_APP_REVISION` on ACA; this package does not map it to the OTLP resource yet.)

---

## Datadog: services, logs, and APM hosts

- **Unified tagging:** Traces, metrics, and OTLP logs all use the **same full** OTLP **resource** (service.*, process.*, telemetry SDK, OS, deployment, optional K8s/cloud, `env`, `version`, `ddsource`, etc.). Because the OTel SDK's `WithResource` always merges `resource.Environment()` into the provider resource, this package sanitizes **`OTEL_RESOURCE_ATTRIBUTES`** first so host-like keys and `service.name` cannot be reintroduced by the SDK merge. **Each slog log line** adds a flat top-level **`service`** string (Service facet) plus **high-signal** resource fields only—`env`, `version`, `deployment.environment.name`, `ddsource`, `datadog.log.source`, cloud region, K8s/ACA/container tags from env, and anything from **`OTEL_RESOURCE_ATTRIBUTES`** that is not blocked. Top-level caller-provided **`service`**, **`service.*`**, **`host`**, **`host.*`**, **`hostname`**, and **`datadog.host.name`** attrs are stripped so this package remains authoritative for Datadog host/service mapping. Unrelated nested attrs such as `peer.service` stay intact. **`service.*`**, **`process.*`**, **`telemetry.*`**, **`os.*`**, and deprecated **`deployment.environment`** are **not** repeated on every slog line (avoids argv noise and SDK clutter); those stay on the OTLP resource for correlation. **`trace_id`** / **`span_id`** are added when the log context has an active span ([semantic mapping](https://docs.datadoghq.com/opentelemetry/mapping/semantic_mapping/)).
- **APM / infra host:** This package does **not** set OTLP resource **`datadog.host.name`**. Datadog derives host from its agent, cloud metadata, or other ingest rules ([hostname mapping](https://docs.datadoghq.com/opentelemetry/mapping/hostname/)).
- **`service.instance.id`:** Set from **`CONTAINER_APP_REPLICA_NAME`**, then **`POD_NAME`**, then **`os.Hostname()`**, in that order. Values that look like unexpanded placeholders (`$(VAR)` or `${VAR}`) are skipped so misconfigured hostnames or env vars are not exported to logs. If none apply, a random `instance-…` id is used.
- **`OTEL_RESOURCE_ATTRIBUTES`:** Comma-separated `k=v` pairs merged into the OTLP resource. **Values** that **contain** unexpanded `$(VAR)` / `${…}` placeholders (anywhere in the string) are **dropped** for that pair. Reserved Datadog authority keys (`host`, `host.*`, `hostname`, `datadog.host.name`, bare `service`, `service.name`) are also removed before the SDK sees the env, so they cannot come back during provider resource merges. The same placeholder filter applies to optional K8s/ACA/cloud env vars (`CONTAINER_APP_NAME`, `CONTAINER_NAME`, regions, etc.).
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

After startup, at **debug** level, look for `telemetry initialized` and verify `otlp_logs_enabled`, `otlp_metrics_enabled`, and `otlp_traces_enabled`. That startup event now includes a `resource` group with the effective `service_name`, `service_namespace` when present, `service_version`, `service_instance_id`, and `deployment_environment_name`, which is the fastest way to confirm what this process is putting on the OTLP resource before any collector or backend remaps it. Ensure your ACA environment sends **logs**, **traces**, and **metrics** to Datadog when you expect all three.

---

## Dependency

```bash
go get github.com/hydn-co/telemetry@latest
```

Local multi-repo development:

```text
replace github.com/hydn-co/telemetry => ../telemetry
```
