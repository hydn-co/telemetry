package telemetry

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
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
	res := telemetryResource("mesh-stream", "dev1", "1.2.3")
	next := &captureHandler{}
	logger := slog.New(&correlationHandler{
		next:          next,
		resourceAttrs: resourceToSlogAttrs(res),
		serviceName:   "mesh-stream",
	})

	logger.Info("hello")

	if len(next.records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(next.records))
	}

	attrs := recordAttrs(next.records[0])
	if attrs["service.name"] != "mesh-stream" {
		t.Fatalf("expected service.name attr, got %q", attrs["service.name"])
	}
	if got := serviceGroupName(next.records[0]); got != "mesh-stream" {
		t.Fatalf("expected service group name %q, got %q", "mesh-stream", got)
	}
	if attrs["env"] != "dev1" {
		t.Fatalf("expected env attr, got %q", attrs["env"])
	}
	if attrs["version"] != "1.2.3" {
		t.Fatalf("expected version attr, got %q", attrs["version"])
	}
	if attrs["deployment.environment.name"] != "dev1" {
		t.Fatalf("expected deployment.environment.name attr, got %q", attrs["deployment.environment.name"])
	}
	if attrs["host"] != "" {
		t.Fatalf("did not expect host on log records, got %q", attrs["host"])
	}
	if attrs["ddsource"] != defaultDatadogLogSource {
		t.Fatalf("expected ddsource attr, got %q", attrs["ddsource"])
	}
	if attrs[attrKeyDatadogLogSource] != defaultDatadogLogSource {
		t.Fatalf("expected datadog.log.source attr, got %q", attrs[attrKeyDatadogLogSource])
	}
	if attrs["trace_id"] != "" || attrs["span_id"] != "" {
		t.Fatalf("did not expect trace correlation attrs without a span, got trace_id=%q span_id=%q", attrs["trace_id"], attrs["span_id"])
	}
}

func TestCorrelationHandlerAddsTraceContext(t *testing.T) {
	res := telemetryResource("mesh-stream", "dev1", "1.2.3")
	next := &captureHandler{}
	logger := slog.New(&correlationHandler{
		next:          next,
		resourceAttrs: resourceToSlogAttrs(res),
		serviceName:   "mesh-stream",
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

func TestTelemetryResourceOmitsDatadogHostName(t *testing.T) {
	t.Setenv(EnvOTELResourceAttributes, "datadog.host.name=must-not-appear,foo=kept")
	res := telemetryResource("svc", "dev", "1.0.0")
	var sawFoo bool
	for _, a := range res.Attributes() {
		if string(a.Key) == "datadog.host.name" {
			t.Fatalf("OTLP resource must not set datadog.host.name")
		}
		if string(a.Key) == "foo" && a.Value.AsString() == "kept" {
			sawFoo = true
		}
	}
	if !sawFoo {
		t.Fatal("expected OTEL_RESOURCE_ATTRIBUTES foo=kept to be merged")
	}
}

func TestTelemetryResourceOmitsHostNamespace(t *testing.T) {
	t.Setenv(EnvOTELResourceAttributes, "")
	res := telemetryResource("svc", "dev", "1.0.0")
	for _, a := range res.Attributes() {
		k := string(a.Key)
		if k == "host" || strings.HasPrefix(k, "host.") {
			t.Fatalf("OTLP resource must not set host attributes, got %q", k)
		}
	}
}

func TestAppendResourceAttributesFromEnvParsesPairs(t *testing.T) {
	t.Setenv(EnvOTELResourceAttributes, " deployment.environment.name=qa , foo=bar ")
	out := appendResourceAttributesFromEnv(nil)
	if len(out) != 2 {
		t.Fatalf("expected 2 attrs, got %d", len(out))
	}
	if string(out[0].Key) != "deployment.environment.name" || out[0].Value.AsString() != "qa" {
		t.Fatalf("first attr: %+v", out[0])
	}
}

func TestAppendResourceAttributesFromEnvStripsDatadogHostName(t *testing.T) {
	t.Setenv(EnvOTELResourceAttributes, "datadog.host.name=bad,foo=bar")
	out := appendResourceAttributesFromEnv(nil)
	if len(out) != 1 {
		t.Fatalf("expected 1 attr, got %d", len(out))
	}
	if string(out[0].Key) != "foo" || out[0].Value.AsString() != "bar" {
		t.Fatalf("got %+v", out[0])
	}
}

func TestAppendResourceAttributesFromEnvStripsHostNamespace(t *testing.T) {
	t.Setenv(EnvOTELResourceAttributes, "host.arch=amd64,host.id=x,host=foo,keep=yes")
	out := appendResourceAttributesFromEnv(nil)
	if len(out) != 1 || string(out[0].Key) != "keep" || out[0].Value.AsString() != "yes" {
		t.Fatalf("got %+v", out)
	}
}

func TestAppendResourceAttributesFromEnvSkipsUnexpandedPlaceholderValues(t *testing.T) {
	t.Setenv(EnvOTELResourceAttributes, "k8s.pod.name=$(CONTAINER_APP_REPLICA_NAME),foo=bar,other=${VAR}")
	out := appendResourceAttributesFromEnv(nil)
	if len(out) != 1 || string(out[0].Key) != "foo" || out[0].Value.AsString() != "bar" {
		t.Fatalf("expected only foo=bar, got %+v", out)
	}
}

func TestTelemetryResourceSkipsPlaceholderInOTELResourceAttributes(t *testing.T) {
	t.Setenv(EnvOTELResourceAttributes, "deployment.environment.name=$(CONTAINER_APP_REPLICA_NAME)")
	res := telemetryResource("svc", "dev", "1.0.0")
	for _, a := range res.Attributes() {
		if string(a.Key) == "deployment.environment.name" && a.Value.AsString() == "$(CONTAINER_APP_REPLICA_NAME)" {
			t.Fatal("must not merge unexpanded placeholder into OTLP resource")
		}
	}
}

func TestStripHostResourceAttributes(t *testing.T) {
	in := []attribute.KeyValue{
		attribute.String("service.name", "s"),
		attribute.String("host.arch", "amd64"),
		attribute.String("host.name", "n"),
	}
	out := stripHostResourceAttributes(in)
	if len(out) != 1 || string(out[0].Key) != "service.name" {
		t.Fatalf("got %+v", out)
	}
}

func TestIsUnexpandedSubstitution(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"$(CONTAINER_APP_REPLICA_NAME)", true},
		{"${CONTAINER_APP_REPLICA_NAME}", true},
		{"  $(FOO)  ", true},
		{"my-replica-0000001", false},
		{"", false},
		{"$(unclosed", false},
	}
	for _, tt := range tests {
		if got := isUnexpandedSubstitution(tt.in); got != tt.want {
			t.Errorf("isUnexpandedSubstitution(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestPickServiceInstanceIDPrefersValidReplicaEnv(t *testing.T) {
	t.Setenv(envContainerAppReplica, "aca-replica-xyz")
	t.Setenv(envPodName, "should-not-win")
	if got := pickServiceInstanceID(); got != "aca-replica-xyz" {
		t.Fatalf("got %q", got)
	}
}

func TestPickServiceInstanceIDSkipsPlaceholderReplicaUsesPod(t *testing.T) {
	t.Setenv(envContainerAppReplica, "$(CONTAINER_APP_REPLICA_NAME)")
	t.Setenv(envPodName, "real-pod-name")
	if got := pickServiceInstanceID(); got != "real-pod-name" {
		t.Fatalf("got %q", got)
	}
}

func TestPickServiceInstanceIDSkipsPlaceholderHostnameUsesSynthetic(t *testing.T) {
	t.Setenv(envContainerAppReplica, "$(CONTAINER_APP_REPLICA_NAME)")
	t.Setenv(envPodName, "${POD_NAME}")
	h := pickServiceInstanceID()
	if isUnexpandedSubstitution(h) {
		t.Fatalf("pickServiceInstanceID returned placeholder %q", h)
	}
	if strings.HasPrefix(h, "$(") || strings.HasPrefix(h, "${") {
		t.Fatalf("unexpected %q", h)
	}
}

func TestTelemetryResourceServiceInstanceIDNotLiteralPlaceholder(t *testing.T) {
	t.Setenv(envContainerAppReplica, "$(CONTAINER_APP_REPLICA_NAME)")
	t.Setenv(envPodName, "")
	t.Setenv(EnvOTELResourceAttributes, "")
	res := telemetryResource("svc", "dev", "1.0.0")
	var inst string
	for _, a := range res.Attributes() {
		if a.Key == semconv.ServiceInstanceIDKey {
			inst = a.Value.AsString()
		}
	}
	if inst == "" {
		t.Fatal("missing service.instance.id")
	}
	if isUnexpandedSubstitution(inst) {
		t.Fatalf("service.instance.id must not be a literal placeholder, got %q", inst)
	}
}

func TestResourceAttrsFromEnvSkipsPlaceholderPodName(t *testing.T) {
	t.Setenv(envPodName, "$(POD_NAME)")
	t.Setenv(envContainerAppReplica, "$(CONTAINER_APP_REPLICA_NAME)")
	attrs := resourceAttrsFromEnv()
	for _, a := range attrs {
		if a.Key == semconv.K8SPodNameKey {
			t.Fatalf("expected no k8s.pod.name when env values are placeholders, got %+v", a)
		}
	}
}

func TestResourceAttrsFromEnvUsesPodWhenReplicaPlaceholder(t *testing.T) {
	t.Setenv(envContainerAppReplica, "$(CONTAINER_APP_REPLICA_NAME)")
	t.Setenv(envPodName, "good-pod")
	attrs := resourceAttrsFromEnv()
	var pod string
	for _, a := range attrs {
		if a.Key == semconv.K8SPodNameKey {
			pod = a.Value.AsString()
		}
	}
	if pod != "good-pod" {
		t.Fatalf("expected k8s.pod.name good-pod, got %q", pod)
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

func serviceGroupName(r slog.Record) string {
	var out string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key != "service" || a.Value.Kind() != slog.KindGroup {
			return true
		}
		for _, ga := range a.Value.Group() {
			if ga.Key == "name" {
				out = ga.Value.String()
				return false
			}
		}
		return true
	})
	return out
}
