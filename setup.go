// Package telemetry provides shared OpenTelemetry setup for mesh services: tracing (OTLP)
// and log-trace correlation via the slog bridge. Configuration is read from standard
// OTEL_* environment variables so the same code can be reused across projects.
//
// The function returned by Setup is safe to call multiple times; call it once on process exit (e.g. defer).
package telemetry

import (
	"context"
	"log/slog"
	"os"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
)

// Environment variable names read by this package.
// Set by deployment (e.g. when running with a Datadog agent sidecar).
const (
	EnvOTELExporterOTLPEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"
	EnvOTELServiceName          = "OTEL_SERVICE_NAME"
)

// Options configures Setup. ServiceName is used for the OTel resource and slog
// correlation; if empty, OTEL_SERVICE_NAME from env or "unknown" is used.
type Options struct {
	ServiceName string // e.g. "mesh-stream"
	Version     string // optional; set on OTel resource as service.version and used in shutdown logging
	Environment string // optional; set on OTel resource as deployment.environment (e.g. dev1, prod)
}

// Enabled returns whether OpenTelemetry should be initialized based on environment.
// When true, OTEL_EXPORTER_OTLP_ENDPOINT or OTEL_SERVICE_NAME is set.
func Enabled() bool {
	return os.Getenv(EnvOTELExporterOTLPEndpoint) != "" || os.Getenv(EnvOTELServiceName) != ""
}

// Setup initializes OpenTelemetry tracing and log-trace correlation when OTEL_*
// env vars indicate it (e.g. when running with a Datadog agent sidecar). Otherwise
// returns a no-op shutdown. The returned function should be called on process exit.
func Setup(ctx context.Context, opts Options) func() {
	serviceName := opts.ServiceName
	if serviceName == "" {
		serviceName = os.Getenv(EnvOTELServiceName)
	}
	if serviceName == "" {
		serviceName = "unknown"
	}
	version := opts.Version

	if !Enabled() {
		return func() {} // no-op; safe to call multiple times
	}

	// OTLP gRPC exporter (reads OTEL_EXPORTER_OTLP_ENDPOINT from env when not in options)
	exp, err := otlptracegrpc.New(ctx)
	if err != nil {
		slog.Error("failed to create OTLP trace exporter", slog.String("error", err.Error()))
		return func() {}
	}

	attrs := []attribute.KeyValue{semconv.ServiceNameKey.String(serviceName)}
	if opts.Environment != "" {
		attrs = append(attrs, semconv.DeploymentEnvironmentKey.String(opts.Environment))
	}
	if version != "" {
		attrs = append(attrs, semconv.ServiceVersionKey.String(version))
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
	slog.SetDefault(otelslog.NewLogger(serviceName))

	return func() {
		slog.InfoContext(ctx, "telemetry shutdown",
			slog.String("service", serviceName),
			slog.String("version", version),
		)
		// Use a fresh context so shutdown/flush runs even if the caller's ctx is already cancelled.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := tp.Shutdown(shutdownCtx); err != nil {
			slog.ErrorContext(ctx, "tracer provider shutdown", slog.String("error", err.Error()))
		}
	}
}
