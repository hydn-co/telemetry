# hydn-co/telemetry

Shared OpenTelemetry bootstrap for Go services that export **traces**, **metrics**, and **logs** over **OTLP/gRPC** (insecure) to a local endpoint—primarily **Azure Container Apps** managed OpenTelemetry forwarding to **Datadog**.

It also installs a correlated **`log/slog`** pipeline: JSON to stdout or stderr, optional file copy, OTLP log export, and fields suited to Datadog log–trace correlation.

**Callers** should only invoke `telemetry.Setup` when their app is configured for OTLP (for example after checking a service-specific `*_TELEMETRY_ENABLED` env var). This package does not read those flags.

---

## What `Setup` does

1. **Validates** required `OTEL_*` env vars (panics if any are missing).
2. Builds an OTLP **resource** (service, deployment, process, OS, optional K8s/ACA attributes). The resource **never** sets **host** semantic attributes (`host`, `host.*`, `datadog.host.name`); those keys are stripped from `OTEL_RESOURCE_ATTRIBUTES` if present.
3. Installs **slog**: primary JSON sink (stdout by default, or `Options.PrimaryLogWriter`), optional `LOG_FILE`, correlation fields (including top-level **`service`** for Datadog), OTLP log bridge (`otelslog` scope name + version from `OTEL_SERVICE_*`).
4. Registers global **MeterProvider** (OTLP metrics) and **TracerProvider** (OTLP traces). The tracer provider also stamps Datadog's OTLP compatibility attribute **`analytics.event=true`** on **`server`** and **`consumer`** spans so Datadog Trace Analytics works without `dd-trace-go`.
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
| `OTEL_TRACES_SAMPLER` | Optional OpenTelemetry trace sampler. Supported values: `always_on`, `always_off`, `traceidratio`, `parentbased_always_on`, `parentbased_always_off`, `parentbased_traceidratio`. Default: `parentbased_always_on`. |
| `OTEL_TRACES_SAMPLER_ARG` | Argument for ratio-based samplers (`traceidratio`, `parentbased_traceidratio`). Must be a float from `0` to `1`. Invalid values fall back to `1`. |
| `OTEL_GIT_REPOSITORY_URL` / `DD_GIT_REPOSITORY_URL` | Repo URL for Datadog Source Code Integration + Deployment Tracking. First non-empty, non-placeholder value wins; emitted on the OTLP resource as `git.repository_url`. |
| `OTEL_GIT_COMMIT_SHA` / `DD_GIT_COMMIT_SHA` | Full commit SHA for SCI + Deployment Tracking. First non-empty, non-placeholder value wins; emitted as `git.commit.sha`. |

### Optional resource hints (read if set)

Used for OTLP resource enrichment only; see `setup.go` (`resourceAttrsFromEnv`):

`OTEL_SERVICE_NAMESPACE`, `POD_NAME`, `POD_NAMESPACE`, `POD_UID`, `NODE_NAME`, `CONTAINER_NAME`, `CONTAINER_APP_NAME`, `CONTAINER_APP_REPLICA_NAME`, `AWS_REGION`, `AZURE_REGION`, `GOOGLE_CLOUD_REGION`. (Azure also sets `CONTAINER_APP_REVISION` on ACA; this package does not map it to the OTLP resource yet.)

---

## Datadog: services, logs, and APM hosts

- **Unified tagging:** Traces, metrics, and OTLP logs all use the **same full** OTLP **resource** (service.*, process.*, telemetry SDK, OS, deployment, optional K8s/cloud, `env`, `version`, `ddsource`, etc.). Because the OTel SDK's `WithResource` always merges `resource.Environment()` into the provider resource, this package sanitizes **`OTEL_RESOURCE_ATTRIBUTES`** first so host-like keys and `service.name` cannot be reintroduced by the SDK merge. **Each slog log line** adds a flat top-level **`service`** string (Service facet) plus **high-signal** resource fields only—`env`, `version`, `deployment.environment.name`, `ddsource`, `datadog.log.source`, cloud region, K8s/ACA/container tags from env, and anything from **`OTEL_RESOURCE_ATTRIBUTES`** that is not blocked. Top-level caller-provided **`service`**, **`service.*`**, **`host`**, **`host.*`**, **`hostname`**, and **`datadog.host.name`** attrs are stripped so this package remains authoritative for Datadog host/service mapping. Unrelated nested attrs such as `peer.service` stay intact. **`service.*`**, **`process.*`**, **`telemetry.*`**, **`os.*`**, and deprecated **`deployment.environment`** are **not** repeated on every slog line (avoids argv noise and SDK clutter); those stay on the OTLP resource for correlation. **`trace_id`** / **`span_id`** are added when the log context has an active span ([semantic mapping](https://docs.datadoghq.com/opentelemetry/mapping/semantic_mapping/)).
- **Trace Search / Analytics:** This package emits Datadog's OTLP compatibility attribute **`analytics.event=true`** on **`server`** and **`consumer`** spans by default so legacy Trace Analytics facets stay populated in Datadog even though the service is instrumented with OpenTelemetry instead of `dd-trace-go`. Trace volume is still controlled by the OpenTelemetry sampler in this process (`OTEL_TRACES_SAMPLER`, default `parentbased_always_on`) and by downstream Datadog Agent / Collector ingestion controls.
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

After startup, at **debug** level, look for `telemetry initialized` and verify `otlp_logs_enabled`, `otlp_metrics_enabled`, `otlp_traces_enabled`, `trace_sampler`, `datadog_trace_analytics_enabled`, and `datadog_trace_analytics_span_kinds`. That startup event also includes a `resource` group with the effective `service_name`, `service_namespace` when present, `service_version`, `service_instance_id`, and `deployment_environment_name`, which is the fastest way to confirm what this process is putting on the OTLP resource before any collector or backend remaps it. Ensure your ACA environment sends **logs**, **traces**, and **metrics** to Datadog when you expect all three.

---

## Datadog billing stance

This package is **deliberately conservative** about the OTLP attributes that can materialize **billed** Datadog entities. Infrastructure Hosts, Container Monitoring, and APM Hosts are separate Datadog SKUs; each can be triggered by specific OTLP resource attributes.

- **No `host.*` / `hostname` / `datadog.host.name`.** Stripped from `OTEL_RESOURCE_ATTRIBUTES` and never set by this package. Prevents an Infrastructure Host entry per process/replica.
- **No `container.id`.** Not auto-detected from cgroups. Prevents an APM Host or Container Monitoring entity per replica.
- **No `container.image.name` / `container.image.tag`.** Not emitted. Prevents entries flowing into the Containers product.

Consequence: The **Infrastructure** panel on a service's APM page will be blank. All other APM features (Catalog, Summary, Traces, Version/Deployment Tracking, log correlation) work because they key off `service.name`, `service.version`, `deployment.environment.name`, `env`, `version`, `git.repository_url`, and `git.commit.sha`, none of which add host/container billing.

If you later decide to accept the billing trade-off for infra correlation, add `container.id` / `host.name` via a dedicated code path — do not re-enable them through `OTEL_RESOURCE_ATTRIBUTES`, since the sanitizer will strip them.

---

## Datadog Service Catalog / Service Summary / Version Tracking is empty?

Assuming traces actually arrive in Datadog (filter Trace Explorer by `service:<your-service>`), work through this checklist:

1. **APM stats are not being computed for your OTLP spans.** This is the most common cause of empty **Service Summary** panels (requests / latency / errors). Datadog only shows these when APM stats are computed from top-level spans. Server and consumer spans are auto-top-level; this package also stamps `_dd.measured=1` on them so the Datadog ingest marks them as measured explicitly. If panels are still empty, the ingest side is dropping or not converting spans:
   - **OTel Collector with Datadog exporter:** set `exporters.datadog.traces.compute_stats_by_span_kind: true` and confirm a `datadog` connector or trace pipeline is present.
   - **Datadog Agent OTLP receiver:** set `DD_APM_FEATURES=enable_otlp_compute_top_level_by_span_kind` on the agent.
   - **Azure Container Apps managed OpenTelemetry:** confirm the Datadog destination is wired for **traces** (APM), not only logs and metrics. ACA's managed destination forwards to the Datadog Exporter; without APM wired, spans are discarded at ingest.
2. **Version Tracking / Deployments tab is empty.** Requires git metadata on the OTLP resource:
   - Set `OTEL_GIT_REPOSITORY_URL` and `OTEL_GIT_COMMIT_SHA` (or `DD_GIT_*` fallbacks) at build / deploy time. In CI: `OTEL_GIT_COMMIT_SHA=$(git rev-parse HEAD)` and the clone URL.
   - `service.version` alone populates the version tag but does not draw the Deployments timeline.
3. **Service Catalog entry is missing or thin.** Auto-discovered catalog entries appear from APM traces with `service.name`. For richer metadata (team, on-call, links, tier), commit a `service.datadog.yaml` **per service repo** (not in this shared library).
4. **Replicas look merged into one entity.** `service.instance.id` is set from `CONTAINER_APP_REPLICA_NAME` -> `POD_NAME` -> `os.Hostname()`. If your ACA `CONTAINER_APP_REPLICA_NAME` is the literal `$(CONTAINER_APP_REPLICA_NAME)` placeholder, it is intentionally skipped — fix the env injection so the value expands.

---

## Dependency

```bash
go get github.com/hydn-co/telemetry@latest
```

Local multi-repo development:

```text
replace github.com/hydn-co/telemetry => ../telemetry
```
