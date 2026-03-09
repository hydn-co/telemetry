// Package telemetry provides shared OpenTelemetry tracing (OTLP) and structured
// JSON slog logging with Datadog-friendly correlation. Configuration is read from
// OTEL_* and LOG_FILE environment variables so the same code can be reused across projects.
//
// Primary logging is standard slog JSON to stdout (and optionally to a file when
// LOG_FILE is set). Each log record gets service, env, and version from env, and
// trace_id/span_id when the log context has an active span. No OpenTelemetry logs
// export is required; the Datadog agent can tail the log file or collect stdout.
//
// The function returned by Setup is safe to call multiple times; call it once on process exit (e.g. defer).
package telemetry

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
)

// EnvLogFile is the optional path for file-based log collection (e.g. /LogFiles/app.log for Datadog sidecar).
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

func (h *correlationHandler) Handle(ctx context.Context, r slog.Record) error {
	rec := r.Clone()
	rec.AddAttrs(
		slog.String("service", h.serviceName),
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

// Setup initializes structured JSON logging with correlation fields, then OpenTelemetry
// tracing. Required env vars OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_SERVICE_NAME,
// OTEL_DEPLOYMENT_ENVIRONMENT, and OTEL_SERVICE_VERSION must be set; Setup panics otherwise.
// Logging is installed first so it works even if tracer setup fails. The returned
// function should be called on process exit (it shuts down the tracer and closes the log file if set).
func Setup(ctx context.Context, opts Options) func() {
	requireOTELEnv()

	serviceName := os.Getenv(EnvOTELServiceName)
	environment := os.Getenv(EnvOTELDeploymentEnvironment)
	version := os.Getenv(EnvOTELServiceVersion)

	// Primary logging: JSON to stdout (and optionally to LOG_FILE). Install before tracer
	// so logs work even when tracer setup fails.
	stdoutHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	var handler slog.Handler = stdoutHandler
	var logFile *os.File
	if logPath := os.Getenv(EnvLogFile); logPath != "" {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			// Log before installing our handler so any configured logger sees it
			slog.Default().Error("telemetry: failed to open log file for Datadog tailing", "path", logPath, "error", err)
		} else {
			logFile = f
			fileHandler := slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug})
			handler = &multiHandler{handlers: []slog.Handler{stdoutHandler, fileHandler}}
		}
	}
	handler = &correlationHandler{next: handler, serviceName: serviceName, env: environment, version: version}
	slog.SetDefault(slog.New(handler))

	// OTLP tracing (best-effort; logging already works).
	// WithInsecure() is required for the local Datadog agent sidecar (plain gRPC on 127.0.0.1:4317).
	exp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithInsecure())
	if err != nil {
		slog.Error("failed to create OTLP trace exporter", slog.String("error", err.Error()))
		return makeShutdownFunc(nil, logFile, serviceName, version, environment)
	}

	attrs := []attribute.KeyValue{
		semconv.ServiceNameKey.String(serviceName),
		semconv.DeploymentEnvironmentKey.String(environment),
		semconv.ServiceVersionKey.String(version),
	}
	res := resource.NewWithAttributes(semconv.SchemaURL, attrs...)

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	return makeShutdownFunc(tp, logFile, serviceName, version, environment)
}

func makeShutdownFunc(tp *sdktrace.TracerProvider, logFile *os.File, serviceName, version, environment string) func() {
	return func() {
		slog.InfoContext(context.Background(), "telemetry shutdown",
			slog.String("service", serviceName),
			slog.String("version", version),
			slog.String("environment", environment),
		)
		if logFile != nil {
			_ = logFile.Close()
		}
		if tp != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := tp.Shutdown(shutdownCtx); err != nil {
				slog.Error("tracer provider shutdown", slog.String("error", err.Error()))
			}
		}
	}
}
