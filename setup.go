// Package telemetry configures OpenTelemetry (OTLP/gRPC traces, metrics, logs) and a
// correlated default slog logger for services that send telemetry to a local collector,
// typically Azure Container Apps managed OpenTelemetry → Datadog.
//
// See the repository README for environment variables, Datadog host coalescing
// (datadog.host.name), and usage. Callers should invoke [Setup] only when OTLP is
// enabled for the process; this package does not read application-specific telemetry flags.
//
// The shutdown function returned by [Setup] is idempotent; defer it on exit.
package telemetry

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otlploggrpc "go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	otlpmetricgrpc "go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otellogglobal "go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
)

// EnvLogFile is the optional path for file-based log duplication when another
// collector expects a file path (for example, /LogFiles/app.log).
const EnvLogFile = "LOG_FILE"

// Environment variable names read by this package.
// All four are required; Setup panics if any are missing (fail fast).
const (
	EnvOTELExporterOTLPEndpoint  = "OTEL_EXPORTER_OTLP_ENDPOINT"
	EnvOTELServiceName           = "OTEL_SERVICE_NAME"
	EnvOTELDeploymentEnvironment = "OTEL_DEPLOYMENT_ENVIRONMENT"
	EnvOTELServiceVersion        = "OTEL_SERVICE_VERSION"
)

// EnvLogLevel is the optional slog level for the primary JSON sink and OTLP log export
// (same threshold). Values match slog.Level.UnmarshalText (e.g. info, debug, warn).
// Defaults to info when unset or invalid.
const EnvLogLevel = "LOG_LEVEL"

// EnvOTELResourceAttributes is optional comma-separated key=value pairs merged into the
// OTLP resource (e.g. set by Bicep alongside DD_* for Datadog unified tagging).
const EnvOTELResourceAttributes = "OTEL_RESOURCE_ATTRIBUTES"

// EnvDatadogHostName sets OTLP resource attribute datadog.host.name (checked first by
// Datadog hostname resolution). Use to pin billing to a stable logical host.
const EnvDatadogHostName = "DATADOG_HOST_NAME"

// EnvDatadogHostname is an alternate env var for the same override (common Datadog convention).
const EnvDatadogHostname = "DD_HOSTNAME"

// EnvTelemetryDatadogHostPerReplica, when "true" or "1", sets datadog.host.name to the
// process hostname so each ACA/K8s replica counts as its own APM host (legacy behavior;
// expensive on scaled-out Container Apps).
const EnvTelemetryDatadogHostPerReplica = "TELEMETRY_DATADOG_HOST_PER_REPLICA"

// Options configures Setup. Other configuration remains on OTEL_* and LOG_FILE env vars.
type Options struct {
	// PrimaryLogWriter is the destination for the primary JSON slog handler (before the
	// correlation wrapper). Defaults to os.Stdout. Use os.Stderr when stdout must stay
	// reserved (e.g. MCP JSON-RPC on stdout).
	PrimaryLogWriter io.Writer
}

func primarySlogLevel() slog.Level {
	v := strings.TrimSpace(os.Getenv(EnvLogLevel))
	if v == "" {
		return slog.LevelInfo
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(v)); err != nil {
		return slog.LevelInfo
	}
	return level
}

// requireOTELEnv panics if any required OTEL env var is missing (fail fast).
func requireOTELEnv() {
	var missing []string
	if os.Getenv(EnvOTELExporterOTLPEndpoint) == "" {
		missing = append(missing, EnvOTELExporterOTLPEndpoint)
	}
	if os.Getenv(EnvOTELServiceName) == "" {
		missing = append(missing, EnvOTELServiceName)
	}
	if os.Getenv(EnvOTELDeploymentEnvironment) == "" {
		missing = append(missing, EnvOTELDeploymentEnvironment)
	}
	if os.Getenv(EnvOTELServiceVersion) == "" {
		missing = append(missing, EnvOTELServiceVersion)
	}
	if len(missing) > 0 {
		panic("telemetry: missing required environment variables: " + strings.Join(missing, ", "))
	}
}

// defaultDatadogLogSource is the Datadog log integration name (ddsource) for Go services.
const defaultDatadogLogSource = "go"

// correlationHandler adds service, env, version and optionally trace_id/span_id to each record
// so that logs are self-describing for Datadog (unified service tagging + trace correlation).
// It also sets ddsource, datadog.log.source, and deployment.environment.name on every record so
// OTLP/JSON sinks get source and env when ingest ignores resource attributes. Host is not set
// on log records: use OTLP resource datadog.host.name only so replicas do not create extra
// Datadog hosts (see resolveDatadogHostName).
type correlationHandler struct {
	next        slog.Handler
	serviceName string
	env         string
	version     string
}

func (h *correlationHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// Emit Datadog-friendly log attributes: reserved "service"/"env"/"version" for JSON and
// OTLP attribute "service.name" (maps to Service per Datadog semantic mapping).
func (h *correlationHandler) Handle(ctx context.Context, r slog.Record) error {
	rec := recordStripForbiddenHostAttrs(r)
	rec.AddAttrs(
		slog.String("service", h.serviceName),
		slog.String("service.name", h.serviceName),
		slog.String("env", h.env),
		slog.String("version", h.version),
		slog.String("deployment.environment.name", h.env),
		slog.String("ddsource", defaultDatadogLogSource),
		slog.String(attrKeyDatadogLogSource, defaultDatadogLogSource),
	)
	if span := trace.SpanFromContext(ctx); span.SpanContext().IsValid() {
		rec.AddAttrs(
			slog.String("trace_id", span.SpanContext().TraceID().String()),
			slog.String("span_id", span.SpanContext().SpanID().String()),
		)
	}
	return h.next.Handle(ctx, rec)
}

func (h *correlationHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &correlationHandler{next: h.next.WithAttrs(attrs), serviceName: h.serviceName, env: h.env, version: h.version}
}

func (h *correlationHandler) WithGroup(name string) slog.Handler {
	return &correlationHandler{next: h.next.WithGroup(name), serviceName: h.serviceName, env: h.env, version: h.version}
}

// multiHandler forwards each log record to multiple handlers.
type multiHandler struct {
	handlers []slog.Handler
}

func (m *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range m.handlers {
		if err := h.Handle(ctx, r.Clone()); err != nil {
			return err
		}
	}
	return nil
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		next[i] = h.WithAttrs(attrs)
	}
	return &multiHandler{handlers: next}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		next[i] = h.WithGroup(name)
	}
	return &multiHandler{handlers: next}
}

// minLevelHandler gates a wrapped handler by slog level. This keeps OTLP log
// export aligned with the primary JSON sink (LOG_LEVEL) and avoids noise by default.
type minLevelHandler struct {
	next  slog.Handler
	level slog.Level
}

func (h *minLevelHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if level < h.level {
		return false
	}
	return h.next.Enabled(ctx, level)
}

func (h *minLevelHandler) Handle(ctx context.Context, r slog.Record) error {
	return h.next.Handle(ctx, r)
}

func (h *minLevelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &minLevelHandler{next: h.next.WithAttrs(attrs), level: h.level}
}

func (h *minLevelHandler) WithGroup(name string) slog.Handler {
	return &minLevelHandler{next: h.next.WithGroup(name), level: h.level}
}

// Setup initializes structured JSON logging with correlation fields, OpenTelemetry
// log export, metrics export, then OpenTelemetry tracing for ACA-managed localhost
// OTLP. Required env vars OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_SERVICE_NAME,
// OTEL_DEPLOYMENT_ENVIRONMENT, and OTEL_SERVICE_VERSION must be set; Setup panics
// otherwise. Logging is installed before exporter setup so bootstrap failures are
// always emitted through the package logger. The returned function is idempotent
// and should be called on process exit.
func Setup(ctx context.Context, opts Options) func() {
	requireOTELEnv()

	serviceName := os.Getenv(EnvOTELServiceName)
	environment := os.Getenv(EnvOTELDeploymentEnvironment)
	version := os.Getenv(EnvOTELServiceVersion)
	res := telemetryResource(serviceName, environment, version)

	primaryOut := opts.PrimaryLogWriter
	if primaryOut == nil {
		primaryOut = os.Stdout
	}
	logLevel := primarySlogLevel()

	// Primary logging: JSON to PrimaryLogWriter (default stdout) and optionally LOG_FILE.
	// Install before OTLP setup so logs work even when exporters fail.
	primaryHandler := slog.NewJSONHandler(primaryOut, &slog.HandlerOptions{
		Level:       logLevel,
		ReplaceAttr: datadogHostStripReplaceAttr,
	})
	handlers := []slog.Handler{primaryHandler}
	var logFile *os.File
	var logFileErr error
	logPath := os.Getenv(EnvLogFile)
	if logPath != "" {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			logFileErr = err
			// Defer logging until after our base logger is installed so bootstrap
			// failures always use the package's structured logger.
		} else {
			logFile = f
			fileHandler := slog.NewJSONHandler(f, &slog.HandlerOptions{
				Level:       slog.LevelDebug,
				ReplaceAttr: datadogHostStripReplaceAttr,
			})
			handlers = append(handlers, fileHandler)
		}
	}
	slog.SetDefault(newLogger(handlers, serviceName, environment, version))
	if logFileErr != nil {
		slog.Error("telemetry: failed to open log file for Datadog tailing", "path", logPath, "error", logFileErr)
	}

	logProvider, otelLogHandler, err := newOTLPLogHandler(ctx, serviceName, version, res)
	if err != nil {
		slog.Error("failed to create OTLP log exporter", "error", err)
	} else if otelLogHandler != nil {
		handlers = append(handlers, &minLevelHandler{
			next:  &stripDatadogHostHandler{next: otelLogHandler},
			level: logLevel,
		})
	}
	metricProvider, err := newOTLPMetricProvider(ctx, res)
	if err != nil {
		slog.Error("failed to create OTLP metric exporter", "error", err)
	}
	slog.SetDefault(newLogger(handlers, serviceName, environment, version))
	primaryDesc := "stdout"
	if primaryOut == os.Stderr {
		primaryDesc = "stderr"
	}
	slog.Debug("telemetry initialized",
		"primary_log", primaryDesc,
		"log_file", logPath,
		"otlp_logs_enabled", logProvider != nil,
		"otlp_metrics_enabled", metricProvider != nil,
	)

	// OTLP tracing (best-effort; logging already works).
	// WithInsecure() is used for plain gRPC to the ACA-managed OTLP endpoint (e.g. 127.0.0.1:4317).
	exp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithInsecure())
	if err != nil {
		slog.Error("failed to create OTLP trace exporter", slog.String("error", err.Error()))
		return makeShutdownFunc(nil, logProvider, metricProvider, logFile, serviceName, version, environment)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	return makeShutdownFunc(tp, logProvider, metricProvider, logFile, serviceName, version, environment)
}

func newLogger(handlers []slog.Handler, serviceName, environment, version string) *slog.Logger {
	var handler slog.Handler = handlers[0]
	if len(handlers) > 1 {
		handler = &multiHandler{handlers: handlers}
	}
	handler = &correlationHandler{next: handler, serviceName: serviceName, env: environment, version: version}
	return slog.New(handler)
}

// attrKeyDatadogLogSource is the resource attribute Datadog uses to set the log
// source (ddsource). When set on the OTLP resource, the agent maps it so
// source is defined in Datadog instead of "undefined".
const attrKeyDatadogLogSource = "datadog.log.source"

// otelSDKVersion is the OpenTelemetry Go SDK version used for telemetry.sdk.version
// resource attribute. Matches go.opentelemetry.io/otel/sdk in go.mod.
const otelSDKVersion = "1.42.0"

// Optional environment variables for resource attributes (K8s, ACA, cloud).
// When set, the corresponding semconv attributes are added to the resource.
const (
	envServiceNamespace     = "OTEL_SERVICE_NAMESPACE"
	envPodName              = "POD_NAME"
	envPodNamespace         = "POD_NAMESPACE"
	envPodUID               = "POD_UID"
	envNodeName             = "NODE_NAME"
	envContainerName        = "CONTAINER_NAME"
	envContainerAppName     = "CONTAINER_APP_NAME"
	envContainerAppReplica  = "CONTAINER_APP_REPLICA_NAME"
	envContainerAppRevision = "CONTAINER_APP_REVISION"
	envAWSRegion            = "AWS_REGION"
	envAzureRegion          = "AZURE_REGION"
	envGCPRegion            = "GOOGLE_CLOUD_REGION"
)

func telemetryResource(serviceName, environment, version string) *resource.Resource {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "unknown"
	}
	// Empty schemaURL avoids some pipelines expanding resource into nested objects that
	// confuse Datadog's Service facet. Use standard OTel resource keys only: Datadog maps
	// service.name → service, service.version → version, deployment.environment.name → env
	// (see https://docs.datadoghq.com/opentelemetry/mapping/semantic_mapping/).
	attrs := []attribute.KeyValue{
		semconv.ServiceName(serviceName),
		semconv.ServiceVersion(version),
		semconv.ServiceInstanceID(hostname),
		// Deployment (deprecated key + name for newer Datadog Agent / OTLP mapping)
		semconv.DeploymentEnvironmentKey.String(environment),
		attribute.String("deployment.environment.name", environment),
		// Host (do not set host.name in ACA — os.Hostname() is replica hostname and fragments Datadog hosts)
		semconv.HostArchKey.String(runtime.GOARCH),
		// Process
		semconv.ProcessPID(os.Getpid()),
		semconv.ProcessRuntimeName("go"),
		semconv.ProcessRuntimeVersion(runtime.Version()),
		semconv.ProcessRuntimeDescription("go version " + runtime.Version()),
		// Telemetry SDK
		semconv.TelemetrySDKName("opentelemetry"),
		semconv.TelemetrySDKLanguageKey.String("go"),
		semconv.TelemetrySDKVersion(otelSDKVersion),
		// Datadog
		attribute.String(attrKeyDatadogLogSource, defaultDatadogLogSource),
	}
	// Process: executable name/path, command line, parent PID (best-effort)
	if name := processExecutableName(); name.Key != "" {
		attrs = append(attrs, name)
	}
	if path, err := os.Executable(); err == nil {
		attrs = append(attrs, semconv.ProcessExecutablePath(path))
	}
	if len(os.Args) > 0 {
		attrs = append(attrs, semconv.ProcessCommand(os.Args[0]))
		attrs = append(attrs, semconv.ProcessCommandLine(strings.Join(os.Args, " ")))
		if len(os.Args) > 1 {
			attrs = append(attrs, semconv.ProcessCommandArgs(os.Args[1:]...))
		}
	}
	if ppid := os.Getppid(); ppid > 0 {
		attrs = append(attrs, semconv.ProcessParentPID(ppid))
	}
	// OS (runtime provides GOOS)
	attrs = append(attrs, semconv.OSName(runtime.GOOS))
	// Optional: service.namespace
	if v := os.Getenv(envServiceNamespace); v != "" {
		attrs = append(attrs, semconv.ServiceNamespace(v))
	}
	// Optional: Kubernetes / Azure Container Apps (from env)
	attrs = append(attrs, resourceAttrsFromEnv()...)
	// Optional: Cloud region
	if v := os.Getenv(envAWSRegion); v != "" {
		attrs = append(attrs, semconv.CloudRegion(v))
	} else if v := os.Getenv(envAzureRegion); v != "" {
		attrs = append(attrs, semconv.CloudRegion(v))
	} else if v := os.Getenv(envGCPRegion); v != "" {
		attrs = append(attrs, semconv.CloudRegion(v))
	}
	attrs = appendResourceAttributesFromEnv(attrs)
	attrs = upsertStringAttr(attrs, "datadog.host.name", resolveDatadogHostName(serviceName, environment))
	return resource.NewWithAttributes("", attrs...)
}

// resolveDatadogHostName returns the Datadog APM/Infrastructure hostname for OTLP resources.
// Default is "{serviceName}-{environment}" so all replicas of the same service share one
// billable host. Override with DATADOG_HOST_NAME or DD_HOSTNAME; opt into per-replica hosts
// with TELEMETRY_DATADOG_HOST_PER_REPLICA=true (see https://docs.datadoghq.com/opentelemetry/mapping/hostname/).
func resolveDatadogHostName(serviceName, environment string) string {
	for _, k := range []string{EnvDatadogHostName, EnvDatadogHostname} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	if telemetryDatadogHostPerReplica() {
		h, _ := os.Hostname()
		if strings.TrimSpace(h) == "" {
			return "unknown"
		}
		return h
	}
	return serviceName + "-" + environment
}

func telemetryDatadogHostPerReplica() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(EnvTelemetryDatadogHostPerReplica)))
	return v == "true" || v == "1"
}

func upsertStringAttr(attrs []attribute.KeyValue, key string, v string) []attribute.KeyValue {
	k := attribute.Key(key)
	out := make([]attribute.KeyValue, 0, len(attrs)+1)
	for _, a := range attrs {
		if a.Key != k {
			out = append(out, a)
		}
	}
	out = append(out, attribute.String(key, v))
	return out
}

// appendResourceAttributesFromEnv parses OTEL_RESOURCE_ATTRIBUTES (comma-separated k=v)
// and appends string attributes. Duplicate keys are allowed; last writer wins in some
// backends—call after base attrs so env can supplement Bicep-provided resource hints.
func appendResourceAttributesFromEnv(attrs []attribute.KeyValue) []attribute.KeyValue {
	raw := strings.TrimSpace(os.Getenv(EnvOTELResourceAttributes))
	if raw == "" {
		return attrs
	}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			continue
		}
		k := strings.TrimSpace(kv[0])
		v := strings.TrimSpace(kv[1])
		if k == "" {
			continue
		}
		attrs = append(attrs, attribute.String(k, v))
	}
	return attrs
}

// resourceAttrsFromEnv returns semconv attributes for K8s/ACA/container when the
// corresponding environment variables are set. Pod name uses POD_NAME or
// CONTAINER_APP_REPLICA_NAME only; do not invent a pod name from hostname.
func resourceAttrsFromEnv() []attribute.KeyValue {
	var out []attribute.KeyValue
	podName := os.Getenv(envPodName)
	if podName == "" {
		podName = os.Getenv(envContainerAppReplica)
	}
	if podName != "" {
		out = append(out, semconv.K8SPodName(podName))
	}
	if v := os.Getenv(envPodNamespace); v != "" {
		out = append(out, semconv.K8SNamespaceName(v))
	}
	if v := os.Getenv(envPodUID); v != "" {
		out = append(out, semconv.K8SPodUID(v))
	}
	if v := os.Getenv(envNodeName); v != "" {
		out = append(out, semconv.K8SNodeName(v))
	}
	if v := os.Getenv(envContainerAppName); v != "" {
		out = append(out, semconv.K8SDeploymentName(v))
	}
	if v := os.Getenv(envContainerName); v != "" {
		out = append(out, semconv.ContainerName(v))
		out = append(out, semconv.K8SContainerName(v))
	}
	// CONTAINER_APP_REVISION has no direct semconv; could add as custom or omit
	return out
}

// processExecutableName returns the process executable name (semconv process.executable.name).
// Returns a zero KeyValue on error so callers can skip adding it.
func processExecutableName() attribute.KeyValue {
	path, err := os.Executable()
	if err != nil {
		return attribute.KeyValue{}
	}
	return semconv.ProcessExecutableName(filepath.Base(path))
}

func newOTLPLogHandler(ctx context.Context, serviceName, version string, res *resource.Resource) (*sdklog.LoggerProvider, slog.Handler, error) {
	exp, err := otlploggrpc.New(ctx, otlploggrpc.WithInsecure())
	if err != nil {
		return nil, nil, err
	}

	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)),
		sdklog.WithResource(res),
	)
	otellogglobal.SetLoggerProvider(lp)

	// Pass empty service/version so the bridge does not add record-level semconv attributes
	// that the pipeline turns into a nested "service" object. Service/env/version come from
	// the resource and from correlationHandler on each record (flat "service" for Datadog).
	handler := otelslog.NewHandler(
		"",
		otelslog.WithLoggerProvider(lp),
		otelslog.WithVersion(""),
	)
	return lp, handler, nil
}

func newOTLPMetricProvider(ctx context.Context, res *resource.Resource) (*sdkmetric.MeterProvider, error) {
	exp, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithInsecure())
	if err != nil {
		return nil, err
	}

	reader := sdkmetric.NewPeriodicReader(exp)
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)
	return mp, nil
}

func makeShutdownFunc(tp *sdktrace.TracerProvider, lp *sdklog.LoggerProvider, mp *sdkmetric.MeterProvider, logFile *os.File, serviceName, version, environment string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			slog.InfoContext(context.Background(), "telemetry shutdown",
				slog.String("service", serviceName),
				slog.String("version", version),
				slog.String("environment", environment),
			)
			if tp != nil {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := tp.Shutdown(shutdownCtx); err != nil {
					slog.Error("tracer provider shutdown", slog.String("error", err.Error()))
				}
			}
			if mp != nil {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := mp.Shutdown(shutdownCtx); err != nil {
					slog.Error("meter provider shutdown", slog.String("error", err.Error()))
				}
			}
			if lp != nil {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := lp.Shutdown(shutdownCtx); err != nil {
					slog.Error("logger provider shutdown", slog.String("error", err.Error()))
				}
			}
			if logFile != nil {
				_ = logFile.Close()
			}
		})
	}
}
