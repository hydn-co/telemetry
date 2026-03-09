// Package telemetry provides shared OpenTelemetry setup for mesh services: tracing (OTLP)
// and log-trace correlation via the slog bridge. Configuration is read from standard
// OTEL_* environment variables so the same code can be reused across projects.
//
// When LOG_FILE is set, logs are also written to that path (JSON, one record per line)
// for Datadog sidecar file tailing. The file is opened append-only and shared with the sidecar
// via a shared volume (e.g. /LogFiles/app.log).
//
// The function returned by Setup is safe to call multiple times; call it once on process exit (e.g. defer).
package telemetry

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
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

// Setup initializes OpenTelemetry tracing and log-trace correlation. Required env vars
// OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_SERVICE_NAME, OTEL_DEPLOYMENT_ENVIRONMENT, and
// OTEL_SERVICE_VERSION must be set; Setup panics otherwise (fail fast). The returned
// function should be called on process exit.
func Setup(ctx context.Context, opts Options) func() {
	requireOTELEnv()

	serviceName := os.Getenv(EnvOTELServiceName)
	environment := os.Getenv(EnvOTELDeploymentEnvironment)
	version := os.Getenv(EnvOTELServiceVersion)

	// OTLP gRPC exporter (reads OTEL_EXPORTER_OTLP_ENDPOINT from env when not in options)
	exp, err := otlptracegrpc.New(ctx)
	if err != nil {
		slog.Error("failed to create OTLP trace exporter", slog.String("error", err.Error()))
		return func() {}
	}

	attrs := []attribute.KeyValue{
		semconv.ServiceNameKey.String(serviceName),
		semconv.DeploymentEnvironmentKey.String(environment),
		semconv.ServiceVersionKey.String(version),
	}
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(semconv.SchemaURL, attrs...),
	)
	if err != nil {
		slog.Error("failed to create OTel resource", slog.String("error", err.Error()))
		_ = exp.Shutdown(ctx)
		return func() {}
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	// Inject trace/span IDs into slog for log-trace correlation (e.g. in Datadog)
	otelLogger := otelslog.NewLogger(serviceName)
	handler := otelLogger.Handler()

	if logPath := os.Getenv(EnvLogFile); logPath != "" {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			slog.Error("failed to open log file for Datadog tailing", slog.String("path", logPath), slog.String("error", err.Error()))
		} else {
			fileHandler := slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug})
			handler = &multiHandler{handlers: []slog.Handler{otelLogger.Handler(), fileHandler}}
		}
	}

	slog.SetDefault(slog.New(handler))

	return func() {
		slog.InfoContext(ctx, "telemetry shutdown",
			slog.String("service", serviceName),
			slog.String("version", version),
			slog.String("environment", environment),
		)
		// Use a fresh context so shutdown/flush runs even if the caller's ctx is already cancelled.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := tp.Shutdown(shutdownCtx); err != nil {
			slog.ErrorContext(ctx, "tracer provider shutdown", slog.String("error", err.Error()))
		}
	}
}
