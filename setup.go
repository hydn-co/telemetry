// Package telemetry provides Azure Container Apps-specific OpenTelemetry setup
// for localhost OTLP export and structured JSON slog logging with Datadog-friendly
// correlation. Configuration is read from OTEL_* and LOG_FILE environment variables
// so the same code can be reused across projects deployed with ACA managed OTLP.
//
// Primary logging is standard slog JSON to stdout (and optionally to a file when
// LOG_FILE is set). Each log record gets service, env, and version from env, and
// trace_id/span_id when the log context has an active span. When OTLP is available,
// slog records are also bridged to OpenTelemetry logs so managed collectors such
// as Azure Container Apps can forward them without a sidecar.
//
// The function returned by Setup is safe to call multiple times; call it once on process exit (e.g. defer).
package telemetry

import (
	"context"
	"log/slog"
	"os"
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

// Options is reserved for future use. All configuration is from env vars.
type Options struct{}

// requireOTELEnv panics if any required OTEL env var is missing (fail fast).
// This package assumes ACA injects a localhost OTLP endpoint.
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

// correlationHandler adds service, env, version and optionally trace_id/span_id to each record
// so that logs are self-describing for Datadog (unified service tagging + trace correlation).
type correlationHandler struct {
	next        slog.Handler
	serviceName string
	env         string
	version     string
}

func (h *correlationHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// attrServiceName is a flat string attribute for the service name. Pipelines (e.g. Datadog)
// can map this to the Service facet when "service" is rendered as a nested object from
// OTLP resource (name/version/instance) and the facet expects a string.
const attrServiceName = "service_name"

func (h *correlationHandler) Handle(ctx context.Context, r slog.Record) error {
	rec := r.Clone()
	rec.AddAttrs(
		slog.String("service", h.serviceName),
		slog.String(attrServiceName, h.serviceName), // flat string for pipeline → Service facet
		slog.String("env", h.env),
		slog.String("version", h.version),
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
// export aligned with stdout logging and avoids exporting debug noise by default.
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

	// Primary logging: JSON to stdout (and optionally to LOG_FILE). Install before OTLP
	// setup so logs work even when exporters fail.
	stdoutHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	handlers := []slog.Handler{stdoutHandler}
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
			fileHandler := slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug})
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
		handlers = append(handlers, &minLevelHandler{next: otelLogHandler, level: slog.LevelInfo})
	}
	metricProvider, err := newOTLPMetricProvider(ctx, res)
	if err != nil {
		slog.Error("failed to create OTLP metric exporter", "error", err)
	}
	slog.SetDefault(newLogger(handlers, serviceName, environment, version))
	initLogFile := "stdout only"
	if logPath != "" {
		initLogFile = logPath
	}
	slog.Debug("telemetry initialized",
		"log_file", initLogFile,
		"otlp_logs_enabled", logProvider != nil,
		"otlp_metrics_enabled", metricProvider != nil,
	)

	// OTLP tracing (best-effort; logging already works).
	// WithInsecure() is required for the local Datadog agent sidecar (plain gRPC on 127.0.0.1:4317).
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

func telemetryResource(serviceName, environment, version string) *resource.Resource {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "unknown"
	}
	// Use only flat string attributes so the pipeline doesn't build a nested "service"
	// object that overwrites the Service facet. Datadog maps these to service, env, version, host, source.
	attrs := []attribute.KeyValue{
		attribute.String("service", serviceName),
		attribute.String("version", version),
		semconv.DeploymentEnvironmentKey.String(environment),
		semconv.HostName(hostname),
		attribute.String(attrKeyDatadogLogSource, "go"),
	}
	return resource.NewWithAttributes(semconv.SchemaURL, attrs...)
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
