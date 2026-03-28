// Package telemetry configures OpenTelemetry (OTLP/gRPC traces, metrics, logs) and a
// correlated default slog logger for services that send telemetry to a local collector,
// typically Azure Container Apps managed OpenTelemetry → Datadog.
//
// Datadog-oriented behavior (see https://docs.datadoghq.com/opentelemetry/mapping/semantic_mapping/):
//   - OTLP resource carries the full OpenTelemetry resource (service.*, process.*, telemetry.*, OS,
//     deployment, optional K8s/ACA, etc.); host.* is never emitted.
//   - Each log line adds a flat top-level "service" string (Service facet), strips conflicting
//     top-level user-provided service/service.* and host/host.* attrs, copies high-signal tags
//     from the resource (env, version, ddsource, deployment.environment.name, cloud/K8s/container
//     when set, OTEL_RESOURCE_ATTRIBUTES, …), and adds trace_id/span_id when a span is active.
//   - service.*, process.*, telemetry.*, os.*, and deprecated deployment.environment are not copied
//     onto every slog record (avoids huge command lines and SDK noise on each JSON line). All three
//     providers (trace, metric, log) use the same full OTLP resource so that our correct values
//     override anything the SDK merges from resource.Environment() (e.g. unexpanded ACA
//     placeholders in OTEL_RESOURCE_ATTRIBUTES). Flat per-record tags (service, version, env, …)
//     come from correlationHandler.
//   - Container env DD_SERVICE, DD_ENV, DD_VERSION should match OTEL_*; this package does not read DD_*,
//     but duplicate tagging on the resource from Bicep is fine.
//
// See the repository README for environment variables and usage. Callers should invoke [Setup] only when OTLP is
// enabled for the process; this package does not read application-specific telemetry flags.
//
// The shutdown function returned by [Setup] is idempotent; defer it on exit.
package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
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
// Keys in the OpenTelemetry host namespace (host, host.*), datadog.host.name, and
// service.name are sanitized out so this package stays authoritative for Datadog host
// and service identity across SDK resource merges.
const EnvOTELResourceAttributes = "OTEL_RESOURCE_ATTRIBUTES"

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

// correlationHandler adds Datadog-oriented fields on every record: flat "service", a small set of
// resource-derived tags from resourceToSlogAttrs (not the full resource), and trace_id/span_id when
// a span is active. User records are stripped of reserved Datadog keys so callers cannot override
// service mapping or host mapping (datadog_host_strip.go).
type correlationHandler struct {
	next          slog.Handler
	resourceAttrs []slog.Attr
	serviceName   string
	scopedAttrs   []scopedSlogAttrs
	groups        []string
}

func (h *correlationHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *correlationHandler) Handle(ctx context.Context, r slog.Record) error {
	scoped := cloneScopedSlogAttrs(h.scopedAttrs)
	if attrs := recordReservedDatadogAttrs(h.groups, r); len(attrs) > 0 {
		scoped = append(scoped, scopedSlogAttrs{groups: append([]string(nil), h.groups...), attrs: attrs})
	}
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	out.AddAttrs(materializeScopedSlogAttrs(scoped)...)
	out.AddAttrs(h.resourceAttrs...)
	// Flat string "service" for Datadog Log Explorer Service facet. Do not duplicate service.*
	// resource keys on the record (see resourceToSlogAttrs); Datadog nests them under "service"
	// and breaks the facet.
	out.AddAttrs(slog.String("service", h.serviceName))
	if span := trace.SpanFromContext(ctx); span.SpanContext().IsValid() {
		out.AddAttrs(
			slog.String("trace_id", span.SpanContext().TraceID().String()),
			slog.String("span_id", span.SpanContext().SpanID().String()),
		)
	}
	return h.next.Handle(ctx, out)
}

func (h *correlationHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	filtered := filterReservedDatadogLogAttrs(h.groups, attrs)
	nextScoped := cloneScopedSlogAttrs(h.scopedAttrs)
	if len(filtered) > 0 {
		nextScoped = append(nextScoped, scopedSlogAttrs{groups: append([]string(nil), h.groups...), attrs: append([]slog.Attr(nil), filtered...)})
	}
	return &correlationHandler{next: h.next, resourceAttrs: h.resourceAttrs, serviceName: h.serviceName, scopedAttrs: nextScoped, groups: append([]string(nil), h.groups...)}
}

func (h *correlationHandler) WithGroup(name string) slog.Handler {
	return &correlationHandler{next: h.next, resourceAttrs: h.resourceAttrs, serviceName: h.serviceName, scopedAttrs: cloneScopedSlogAttrs(h.scopedAttrs), groups: appendRawGroup(h.groups, name)}
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
	sanitizeAuthoritativeTelemetryEnvironment()

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
	slog.SetDefault(newLogger(handlers, res, serviceName))
	if logFileErr != nil {
		slog.Error("telemetry: failed to open log file for Datadog tailing", "path", logPath, "error", logFileErr)
	}

	logProvider, otelLogHandler, err := newOTLPLogHandler(ctx, serviceName, version, res)
	if err != nil {
		slog.Error("failed to create OTLP log exporter", "error", err)
	} else if otelLogHandler != nil {
		handlers = append(handlers, &minLevelHandler{
			next:  &stripReservedDatadogAttrsHandler{next: otelLogHandler},
			level: logLevel,
		})
	}
	metricProvider, err := newOTLPMetricProvider(ctx, res)
	if err != nil {
		slog.Error("failed to create OTLP metric exporter", "error", err)
	}
	slog.SetDefault(newLogger(handlers, res, serviceName))
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

func newLogger(handlers []slog.Handler, res *resource.Resource, serviceName string) *slog.Logger {
	var handler slog.Handler = handlers[0]
	if len(handlers) > 1 {
		handler = &multiHandler{handlers: handlers}
	}
	handler = &correlationHandler{
		next:          handler,
		resourceAttrs: resourceToSlogAttrs(res),
		serviceName:   serviceName,
	}
	return slog.New(handler)
}

// resourceToSlogAttrs converts a subset of the OTLP resource into per-log slog attributes for
// stdout/JSON and the OTLP log bridge. Verbose namespaces stay on the resource only (see
// skipResourceKeyOnLogRecords). correlationHandler always adds the flat string "service".
func resourceToSlogAttrs(res *resource.Resource) []slog.Attr {
	if res == nil {
		return nil
	}
	var out []slog.Attr
	for _, kv := range res.Attributes() {
		if skipResourceKeyOnLogRecords(string(kv.Key)) {
			continue
		}
		if kv.Value.Type() == attribute.STRING && isUnexpandedSubstitution(kv.Value.AsString()) {
			continue
		}
		a := otelKeyValueToSlogAttr(kv)
		if a.Key != "" {
			out = append(out, a)
		}
	}
	return out
}

func otelKeyValueToSlogAttr(kv attribute.KeyValue) slog.Attr {
	k := string(kv.Key)
	switch kv.Value.Type() {
	case attribute.INVALID:
		return slog.Attr{}
	case attribute.BOOL:
		return slog.Bool(k, kv.Value.AsBool())
	case attribute.INT64:
		return slog.Int64(k, kv.Value.AsInt64())
	case attribute.FLOAT64:
		return slog.Float64(k, kv.Value.AsFloat64())
	case attribute.STRING:
		return slog.String(k, kv.Value.AsString())
	case attribute.BOOLSLICE:
		return slog.Any(k, kv.Value.AsBoolSlice())
	case attribute.INT64SLICE:
		return slog.Any(k, kv.Value.AsInt64Slice())
	case attribute.FLOAT64SLICE:
		return slog.Any(k, kv.Value.AsFloat64Slice())
	case attribute.STRINGSLICE:
		return slog.Any(k, kv.Value.AsStringSlice())
	default:
		return slog.String(k, kv.Value.AsString())
	}
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
	envServiceNamespace    = "OTEL_SERVICE_NAMESPACE"
	envPodName             = "POD_NAME"
	envPodNamespace        = "POD_NAMESPACE"
	envPodUID              = "POD_UID"
	envNodeName            = "NODE_NAME"
	envContainerName       = "CONTAINER_NAME"
	envContainerAppName    = "CONTAINER_APP_NAME"
	envContainerAppReplica = "CONTAINER_APP_REPLICA_NAME"
	envAWSRegion           = "AWS_REGION"
	envAzureRegion         = "AZURE_REGION"
	envGCPRegion           = "GOOGLE_CLOUD_REGION"
)

// shellOrComposePlaceholder matches "$(VAR)" / "$( VAR )" anywhere in a string (ACA / compose
// copy-paste; some docs use spaces inside parens). bracePlaceholder matches "${...}" (unexpanded
// Bicep-style or env templates). windowsEnvPlaceholder matches unexpanded "%VAR%" (Windows-style refs).
var (
	shellOrComposePlaceholder = regexp.MustCompile(`\$\(\s*[A-Za-z_][A-Za-z0-9_]*\s*\)`)
	bracePlaceholder          = regexp.MustCompile(`\$\{[^}]+\}`)
	windowsEnvPlaceholder     = regexp.MustCompile(`%[A-Za-z_][A-Za-z0-9_]*%`)
)

// isUnexpandedSubstitution reports values that contain shell/compose-style "$(VAR)" or "${...}"
// that was never expanded (e.g. OTEL_RESOURCE_ATTRIBUTES copy-pasted from ACA docs). Matching
// anywhere in the string catches "foo=$(CONTAINER_APP_REPLICA_NAME)" not only a fully-placeholder value.
func isUnexpandedSubstitution(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	return shellOrComposePlaceholder.MatchString(s) || bracePlaceholder.MatchString(s) || windowsEnvPlaceholder.MatchString(s)
}

func pickServiceInstanceID() string {
	for _, k := range []string{envContainerAppReplica, envPodName} {
		v := strings.TrimSpace(os.Getenv(k))
		if v != "" && !isUnexpandedSubstitution(v) {
			return v
		}
	}
	if h, _ := os.Hostname(); h != "" {
		h = strings.TrimSpace(h)
		if !isUnexpandedSubstitution(h) {
			return h
		}
	}
	return fallbackServiceInstanceID()
}

func fallbackServiceInstanceID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "instance-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return "instance-" + hex.EncodeToString(b[:])
}

func podNameFromEnv() string {
	for _, k := range []string{envPodName, envContainerAppReplica} {
		v := strings.TrimSpace(os.Getenv(k))
		if v != "" && !isUnexpandedSubstitution(v) {
			return v
		}
	}
	return ""
}

func telemetryResource(serviceName, environment, version string) *resource.Resource {
	// Empty schemaURL avoids some pipelines expanding resource into nested objects that
	// confuse Datadog's Service facet. Use standard OTel resource keys only: Datadog maps
	// service.name → service, service.version → version, deployment.environment.name → env
	// (see https://docs.datadoghq.com/opentelemetry/mapping/semantic_mapping/).
	attrs := []attribute.KeyValue{
		semconv.ServiceName(serviceName),
		semconv.ServiceVersion(version),
		attribute.String("version", version),
		semconv.ServiceInstanceID(pickServiceInstanceID()),
		// Deployment (deprecated key + name for newer Datadog Agent / OTLP mapping)
		semconv.DeploymentEnvironmentKey.String(environment),
		attribute.String("deployment.environment.name", environment),
		attribute.String("env", environment),
		attribute.String("ddsource", defaultDatadogLogSource),
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
	if v := strings.TrimSpace(os.Getenv(envServiceNamespace)); v != "" && !isUnexpandedSubstitution(v) {
		attrs = append(attrs, semconv.ServiceNamespace(v))
	}
	// Optional: Kubernetes / Azure Container Apps (from env)
	attrs = append(attrs, resourceAttrsFromEnv()...)
	// Optional: Cloud region
	if v := strings.TrimSpace(os.Getenv(envAWSRegion)); v != "" && !isUnexpandedSubstitution(v) {
		attrs = append(attrs, semconv.CloudRegion(v))
	} else if v := strings.TrimSpace(os.Getenv(envAzureRegion)); v != "" && !isUnexpandedSubstitution(v) {
		attrs = append(attrs, semconv.CloudRegion(v))
	} else if v := strings.TrimSpace(os.Getenv(envGCPRegion)); v != "" && !isUnexpandedSubstitution(v) {
		attrs = append(attrs, semconv.CloudRegion(v))
	}
	attrs = appendResourceAttributesFromEnv(attrs)
	attrs = stripHostResourceAttributes(attrs)
	return resource.NewWithAttributes("", attrs...)
}

// skipResourceKeyOnLogRecords reports resource keys we do not copy onto every slog record.
// All three providers (trace, metric, log) now use the full resource so correct values
// override anything the SDK merges from resource.Environment(). This function only controls
// the per-record slog attrs added by correlationHandler — verbose namespaces stay on the
// OTLP resource and are not repeated on every JSON line.
//
// Omitted: host.* (infra host billing), service and service.* (flat "service" string instead),
// process.* (pid, argv, command line bloat), telemetry.* (SDK identity), os.*, and
// deprecated deployment.environment (deployment.environment.name + env are kept).
func skipResourceKeyOnLogRecords(k string) bool {
	if isForbiddenDatadogHostResourceAttributeKey(k) {
		return true
	}
	switch {
	case k == "service":
		return true
	case strings.HasPrefix(k, "service."):
		return true
	case strings.HasPrefix(k, "process."):
		return true
	case strings.HasPrefix(k, "telemetry."):
		return true
	case strings.HasPrefix(k, "os."):
		return true
	case k == "deployment.environment":
		return true
	default:
		return false
	}
}

func isForbiddenDatadogHostResourceAttributeKey(k string) bool {
	path := splitSlogPathComponent(k)
	if len(path) == 0 {
		return false
	}
	return isForbiddenDatadogHostAttrPath(path)
}

// isBlockedDatadogResourceAttributeEnvKey reports OTLP resource env keys we never accept
// from OTEL_RESOURCE_ATTRIBUTES: host-like keys, bare service, and service.name so
// OTEL_SERVICE_NAME remains authoritative.
func isBlockedDatadogResourceAttributeEnvKey(k string) bool {
	if isForbiddenDatadogHostResourceAttributeKey(k) {
		return true
	}
	path := splitSlogPathComponent(k)
	if len(path) == 1 && path[0] == "service" {
		return true
	}
	return len(path) >= 2 && path[0] == "service" && path[1] == "name"
}

func stripHostResourceAttributes(attrs []attribute.KeyValue) []attribute.KeyValue {
	var out []attribute.KeyValue
	for _, a := range attrs {
		if isForbiddenDatadogHostResourceAttributeKey(string(a.Key)) {
			continue
		}
		out = append(out, a)
	}
	return out
}

type otelResourceAttributePair struct {
	key   string
	value string
}

func parseSanitizedOTELResourceAttributes(raw string) []otelResourceAttributePair {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var pairs []otelResourceAttributePair
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
		if k == "" || isBlockedDatadogResourceAttributeEnvKey(k) || isUnexpandedSubstitution(v) {
			continue
		}
		pairs = append(pairs, otelResourceAttributePair{key: k, value: v})
	}
	return pairs
}

func sanitizeOTELResourceAttributes(raw string) string {
	pairs := parseSanitizedOTELResourceAttributes(raw)
	if len(pairs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		parts = append(parts, pair.key+"="+pair.value)
	}
	return strings.Join(parts, ",")
}

func sanitizeAuthoritativeTelemetryEnvironment() {
	sanitized := sanitizeOTELResourceAttributes(os.Getenv(EnvOTELResourceAttributes))
	if sanitized == "" {
		_ = os.Unsetenv(EnvOTELResourceAttributes)
		return
	}
	_ = os.Setenv(EnvOTELResourceAttributes, sanitized)
}

// appendResourceAttributesFromEnv parses OTEL_RESOURCE_ATTRIBUTES (comma-separated k=v)
// and appends string attributes after the authoritative-key filter has been applied.
func appendResourceAttributesFromEnv(attrs []attribute.KeyValue) []attribute.KeyValue {
	for _, pair := range parseSanitizedOTELResourceAttributes(os.Getenv(EnvOTELResourceAttributes)) {
		attrs = append(attrs, attribute.String(pair.key, pair.value))
	}
	return attrs
}

// resourceAttrsFromEnv returns semconv attributes for K8s/ACA/container when the
// corresponding environment variables are set. Pod name uses POD_NAME or
// CONTAINER_APP_REPLICA_NAME only; do not invent a pod name from hostname.
func resourceAttrsFromEnv() []attribute.KeyValue {
	var out []attribute.KeyValue
	if podName := podNameFromEnv(); podName != "" {
		out = append(out, semconv.K8SPodName(podName))
	}
	if v := strings.TrimSpace(os.Getenv(envPodNamespace)); v != "" && !isUnexpandedSubstitution(v) {
		out = append(out, semconv.K8SNamespaceName(v))
	}
	if v := strings.TrimSpace(os.Getenv(envPodUID)); v != "" && !isUnexpandedSubstitution(v) {
		out = append(out, semconv.K8SPodUID(v))
	}
	if v := strings.TrimSpace(os.Getenv(envNodeName)); v != "" && !isUnexpandedSubstitution(v) {
		out = append(out, semconv.K8SNodeName(v))
	}
	if v := strings.TrimSpace(os.Getenv(envContainerAppName)); v != "" && !isUnexpandedSubstitution(v) {
		out = append(out, semconv.K8SDeploymentName(v))
	}
	if v := strings.TrimSpace(os.Getenv(envContainerName)); v != "" && !isUnexpandedSubstitution(v) {
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

	// Scope name/version align with OTEL_SERVICE_NAME / OTEL_SERVICE_VERSION for unified
	// tagging on OTLP logs. Per-record fields (flat service string, env, trace_id, …) come from
	// correlationHandler + resource (service.* resource keys are not duplicated on each record).
	handler := otelslog.NewHandler(
		serviceName,
		otelslog.WithLoggerProvider(lp),
		otelslog.WithVersion(version),
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
