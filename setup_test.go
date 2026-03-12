package telemetry

import (
	"context"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type captureHandler struct {
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &captureHandler{records: h.records}
}

func (h *captureHandler) WithGroup(name string) slog.Handler {
	return &captureHandler{records: h.records}
}

func TestCorrelationHandlerAddsUnifiedTags(t *testing.T) {
	next := &captureHandler{}
	logger := slog.New(&correlationHandler{
		next:        next,
		serviceName: "mesh-stream",
		env:         "dev1",
		version:     "1.2.3",
	})

	logger.Info("hello")

	if len(next.records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(next.records))
	}

	attrs := recordAttrs(next.records[0])
	if attrs["service"] != "mesh-stream" {
		t.Fatalf("expected service attr, got %q", attrs["service"])
	}
	if attrs["env"] != "dev1" {
		t.Fatalf("expected env attr, got %q", attrs["env"])
	}
	if attrs["version"] != "1.2.3" {
		t.Fatalf("expected version attr, got %q", attrs["version"])
	}
	if attrs["trace_id"] != "" || attrs["span_id"] != "" {
		t.Fatalf("did not expect trace correlation attrs without a span, got trace_id=%q span_id=%q", attrs["trace_id"], attrs["span_id"])
	}
}

func TestCorrelationHandlerAddsTraceContext(t *testing.T) {
	next := &captureHandler{}
	logger := slog.New(&correlationHandler{
		next:        next,
		serviceName: "mesh-stream",
		env:         "dev1",
		version:     "1.2.3",
	})

	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
	})

	ctx, span := tp.Tracer("test").Start(context.Background(), "request")
	logger.InfoContext(ctx, "hello")
	span.End()

	if len(next.records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(next.records))
	}

	attrs := recordAttrs(next.records[0])
	if attrs["trace_id"] != span.SpanContext().TraceID().String() {
		t.Fatalf("expected trace_id %q, got %q", span.SpanContext().TraceID().String(), attrs["trace_id"])
	}
	if attrs["span_id"] != span.SpanContext().SpanID().String() {
		t.Fatalf("expected span_id %q, got %q", span.SpanContext().SpanID().String(), attrs["span_id"])
	}
}

func TestMinLevelHandlerFiltersLowerLevels(t *testing.T) {
	next := &captureHandler{}
	logger := slog.New(&minLevelHandler{next: next, level: slog.LevelInfo})

	logger.Debug("drop me")
	logger.Info("keep me")

	if len(next.records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(next.records))
	}
	if next.records[0].Message != "keep me" {
		t.Fatalf("expected info record to be kept, got %q", next.records[0].Message)
	}
}

func TestNewOTLPMetricProviderInstallsGlobalMeterProvider(t *testing.T) {
	prev := otel.GetMeterProvider()
	t.Cleanup(func() {
		otel.SetMeterProvider(prev)
	})

	mp, err := newOTLPMetricProvider(context.Background(), telemetryResource("mesh-stream", "dev1", "1.2.3"))
	if err != nil {
		t.Fatalf("expected metric provider, got error: %v", err)
	}
	t.Cleanup(func() {
		_ = mp.Shutdown(context.Background())
	})

	if mp == nil {
		t.Fatal("expected non-nil meter provider")
	}
	if _, ok := otel.GetMeterProvider().(*sdkmetric.MeterProvider); !ok {
		t.Fatalf("expected global meter provider to be sdk metric provider, got %T", otel.GetMeterProvider())
	}
}

func TestMakeShutdownFuncIsIdempotent(t *testing.T) {
	shutdown := makeShutdownFunc(nil, nil, nil, nil, "mesh-stream", "1.2.3", "dev1")
	shutdown()
	shutdown()
}

func recordAttrs(r slog.Record) map[string]string {
	attrs := make(map[string]string)
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	return attrs
}
