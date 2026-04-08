package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
)

// simulateSDKResourceMerge reproduces what sdklog.WithResource (and sdktrace/sdkmetric)
// do internally: resource.Merge(resource.Environment(), userResource). This is the
// merge that re-introduces raw OTEL_RESOURCE_ATTRIBUTES values the SDK parsed without
// any placeholder detection.
func simulateSDKResourceMerge(t *testing.T, userRes *resource.Resource) *resource.Resource {
	t.Helper()
	merged, err := resource.Merge(resource.Environment(), userRes)
	if err != nil {
		t.Fatal(err)
	}
	return merged
}

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

func TestBootstrapUsesTextFormatAliasesForFallback(t *testing.T) {
	for _, format := range []string{"text", "pretty", "console"} {
		t.Run(format, func(t *testing.T) {
			t.Setenv(envLogFormat, format)

			var buf bytes.Buffer
			prev := slog.Default()
			t.Cleanup(func() {
				slog.SetDefault(prev)
			})

			shutdown := Bootstrap(context.Background(), BootstrapOptions{
				Enabled:          false,
				PrimaryLogWriter: &buf,
			})
			t.Cleanup(shutdown)

			slog.Info("hello world", "component", "shared")

			out := buf.String()
			if !strings.Contains(out, "msg=\"hello world\"") {
				t.Fatalf("expected text slog output, got %q", out)
			}
			if !strings.Contains(out, "component=shared") {
				t.Fatalf("expected text attribute output, got %q", out)
			}
		})
	}
}

func TestBootstrapFallbackDefaultsToJSON(t *testing.T) {
	t.Setenv(envLogFormat, "")

	var buf bytes.Buffer
	prev := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(prev)
	})

	shutdown := Bootstrap(context.Background(), BootstrapOptions{
		Enabled:          false,
		PrimaryLogWriter: &buf,
	})
	t.Cleanup(shutdown)

	slog.Info("hello world", "component", "shared")

	out := buf.String()
	if !strings.Contains(out, "\"msg\":\"hello world\"") {
		t.Fatalf("expected JSON slog output, got %q", out)
	}
	if !strings.Contains(out, "\"component\":\"shared\"") {
		t.Fatalf("expected JSON attribute output, got %q", out)
	}
}

func TestSetOTELServiceIdentityBackfillsUnsetValues(t *testing.T) {
	t.Setenv(EnvOTELServiceName, "")
	t.Setenv(EnvOTELServiceVersion, "")

	setOTELServiceIdentity("mesh-stream", "1.2.3")

	if got := os.Getenv(EnvOTELServiceName); got != "mesh-stream" {
		t.Fatalf("service name = %q, want mesh-stream", got)
	}
	if got := os.Getenv(EnvOTELServiceVersion); got != "1.2.3" {
		t.Fatalf("service version = %q, want 1.2.3", got)
	}
}

func TestSetOTELServiceIdentityPreservesExistingValues(t *testing.T) {
	t.Setenv(EnvOTELServiceName, "existing-service")
	t.Setenv(EnvOTELServiceVersion, "9.9.9")

	setOTELServiceIdentity("mesh-stream", "1.2.3")

	if got := os.Getenv(EnvOTELServiceName); got != "existing-service" {
		t.Fatalf("service name = %q, want existing-service", got)
	}
	if got := os.Getenv(EnvOTELServiceVersion); got != "9.9.9" {
		t.Fatalf("service version = %q, want 9.9.9", got)
	}
}

func TestCorrelationHandlerAddsUnifiedTags(t *testing.T) {
	res := telemetryResource("mesh-stream", "dev1", "1.2.3")
	next := &captureHandler{}
	logger := slog.New(&correlationHandler{
		next:                  next,
		resourceAttrs:         resourceToSlogAttrs(res),
		serviceName:           "mesh-stream",
		deploymentEnvironment: "dev1",
		serviceVersion:        "1.2.3",
	})

	logger.Info("hello")

	if len(next.records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(next.records))
	}

	attrs := recordAttrs(next.records[0])
	if attrs["service.name"] != "" {
		t.Fatalf("did not expect service.name on log record (Datadog nests service.* into a broken Service facet), got %q", attrs["service.name"])
	}
	if attrs["service"] != "mesh-stream" {
		t.Fatalf("expected standard service attr for Datadog facet, got %q", attrs["service"])
	}
	if attrs["dd.service"] != "mesh-stream" || attrs["dd.env"] != "dev1" || attrs["dd.version"] != "1.2.3" {
		t.Fatalf("expected dd.service/dd.env/dd.version for pipeline remappers, got dd.service=%q dd.env=%q dd.version=%q",
			attrs["dd.service"], attrs["dd.env"], attrs["dd.version"])
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
		next:                  next,
		resourceAttrs:         resourceToSlogAttrs(res),
		serviceName:           "mesh-stream",
		deploymentEnvironment: "dev1",
		serviceVersion:        "1.2.3",
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

func TestAppendResourceAttributesFromEnvStripsReservedServiceKey(t *testing.T) {
	t.Setenv(EnvOTELResourceAttributes, "service={\"name\":\"wrong\"},keep=yes")
	out := appendResourceAttributesFromEnv(nil)
	if len(out) != 1 || string(out[0].Key) != "keep" || out[0].Value.AsString() != "yes" {
		t.Fatalf("got %+v", out)
	}
}

func TestAppendResourceAttributesFromEnvStripsServiceName(t *testing.T) {
	t.Setenv(EnvOTELResourceAttributes, "service.name=wrong-service,keep=yes")
	out := appendResourceAttributesFromEnv(nil)
	if len(out) != 1 || string(out[0].Key) != "keep" || out[0].Value.AsString() != "yes" {
		t.Fatalf("got %+v", out)
	}
}

func TestSanitizeOTELResourceAttributesStripsAuthoritativeKeys(t *testing.T) {
	raw := ` host.name = bad , service.name = wrong-service , service={"name":"wrong"}, keep = yes `
	if got := sanitizeOTELResourceAttributes(raw); got != "keep=yes" {
		t.Fatalf("sanitizeOTELResourceAttributes() = %q, want %q", got, "keep=yes")
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
		{"$( CONTAINER_APP_REPLICA_NAME )", true},
		{"${CONTAINER_APP_REPLICA_NAME}", true},
		{"%CONTAINER_APP_REPLICA_NAME%", true},
		{"  $(FOO)  ", true},
		{"prefix=$(CONTAINER_APP_REPLICA_NAME)", true},
		{"attr=${otelEnvironment}", true},
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

func TestResourceToSlogAttrsOmitsVerboseResourceNamespaces(t *testing.T) {
	res := telemetryResource("mesh-stream", "dev1", "1.2.3")
	attrs := resourceToSlogAttrs(res)
	keys := make(map[string]struct{})
	for _, a := range attrs {
		keys[a.Key] = struct{}{}
	}
	for _, banned := range []string{
		"service",
		"service.name",
		"service.version",
		"service.instance.id",
		"process.pid",
		"process.command_line",
		"telemetry.sdk.name",
		"os.name",
		"deployment.environment",
	} {
		if _, ok := keys[banned]; ok {
			t.Fatalf("did not expect %q on per-log resource attrs", banned)
		}
	}
	for _, required := range []string{"env", "version", "ddsource", "deployment.environment.name", attrKeyDatadogLogSource} {
		if _, ok := keys[required]; !ok {
			t.Fatalf("expected %q on per-log resource attrs", required)
		}
	}
}

func TestTelemetryResourceUsesSemconvSchemaAfterSDKMerge(t *testing.T) {
	t.Setenv(EnvOTELServiceName, "mesh-stream")

	res := telemetryResource("mesh-stream", "dev1", "1.2.3")
	if got := res.SchemaURL(); got != semconv.SchemaURL {
		t.Fatalf("resource schema URL = %q, want %q", got, semconv.SchemaURL)
	}

	merged := simulateSDKResourceMerge(t, res)
	if got := merged.SchemaURL(); got != semconv.SchemaURL {
		t.Fatalf("merged resource schema URL = %q, want %q", got, semconv.SchemaURL)
	}
	if svc := resourceStringAttr(merged, semconv.ServiceNameKey); svc != "mesh-stream" {
		t.Fatalf("merged service.name = %q, want mesh-stream", svc)
	}
}

func TestResourceStartupDiagnosticsAttrIncludesMergedServiceIdentity(t *testing.T) {
	t.Setenv(EnvOTELResourceAttributes,
		"service.instance.id=$(CONTAINER_APP_REPLICA_NAME),service.namespace=dev2acaenv4cqolh5yc662y,deployment.environment.name=dev2,service.version=0.1.0-alpha.142")
	t.Setenv(EnvOTELServiceName, "mesh-stream")
	t.Setenv(envContainerAppReplica, "$(CONTAINER_APP_REPLICA_NAME)")
	t.Setenv(envPodName, "dev2meshstream4cqolh5yc662y--0000009-5fbd9cc69-5fbfj")

	res := telemetryResource("mesh-stream", "dev2", "0.1.0-alpha.142")
	merged := simulateSDKResourceMerge(t, res)
	diag := resourceStartupDiagnosticsAttr(merged)
	if diag.Key != "resource" {
		t.Fatalf("expected resource group, got %q", diag.Key)
	}
	got := groupAttrs(diag)
	if got["service_name"] != "mesh-stream" {
		t.Fatalf("service_name = %q, want mesh-stream", got["service_name"])
	}
	if got["service_namespace"] != "dev2acaenv4cqolh5yc662y" {
		t.Fatalf("service_namespace = %q", got["service_namespace"])
	}
	if got["service_version"] != "0.1.0-alpha.142" {
		t.Fatalf("service_version = %q", got["service_version"])
	}
	if got["service_instance_id"] != "dev2meshstream4cqolh5yc662y--0000009-5fbd9cc69-5fbfj" {
		t.Fatalf("service_instance_id = %q", got["service_instance_id"])
	}
	if got["deployment_environment_name"] != "dev2" {
		t.Fatalf("deployment_environment_name = %q", got["deployment_environment_name"])
	}
	if got["k8s_pod_name"] != "dev2meshstream4cqolh5yc662y--0000009-5fbd9cc69-5fbfj" {
		t.Fatalf("k8s_pod_name = %q", got["k8s_pod_name"])
	}
}

func TestLogTelemetryInitializedIncludesTraceStatusAndResourceIdentity(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(prev)
	})
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	res := telemetryResource("mesh-stream", "dev1", "1.2.3")
	logTelemetryInitialized("stderr", "/tmp/app.log", true, true, true, res)

	var obj map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["otlp_traces_enabled"] != true {
		t.Fatalf("expected otlp_traces_enabled=true, got %#v", obj["otlp_traces_enabled"])
	}
	resourceObj, ok := obj["resource"].(map[string]any)
	if !ok {
		t.Fatalf("expected resource group, got %#v", obj["resource"])
	}
	if resourceObj["service_name"] != "mesh-stream" {
		t.Fatalf("expected resource.service_name mesh-stream, got %#v", resourceObj["service_name"])
	}
	if resourceObj["service_version"] != "1.2.3" {
		t.Fatalf("expected resource.service_version 1.2.3, got %#v", resourceObj["service_version"])
	}
	if resourceObj["deployment_environment_name"] != "dev1" {
		t.Fatalf("expected resource.deployment_environment_name dev1, got %#v", resourceObj["deployment_environment_name"])
	}
}

// TestSDKMergeServiceInstanceIDOverridesEnvPlaceholder reproduces the exact bug:
// ACA injects OTEL_RESOURCE_ATTRIBUTES with service.instance.id=$(CONTAINER_APP_REPLICA_NAME).
// The OTel SDK's WithResource merges resource.Environment() (which includes this literal) with
// the resource we provide. Our resource must carry the correct service.instance.id so it wins
// the merge (b overrides a in resource.Merge).
func TestSDKMergeServiceInstanceIDOverridesEnvPlaceholder(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES",
		"service.instance.id=$(CONTAINER_APP_REPLICA_NAME),service.namespace=dev2acaenv,deployment.environment.name=dev2,service.version=1.0.0")
	t.Setenv("OTEL_SERVICE_NAME", "mesh-stream")
	t.Setenv(envContainerAppReplica, "$(CONTAINER_APP_REPLICA_NAME)")
	t.Setenv(envPodName, "dev2meshstream--0000008-5cfcf66475-kztdt")

	res := telemetryResource("mesh-stream", "dev2", "1.0.0")
	merged := simulateSDKResourceMerge(t, res)

	attrs := make(map[attribute.Key]string)
	for _, kv := range merged.Attributes() {
		if kv.Value.Type() == attribute.STRING {
			attrs[kv.Key] = kv.Value.AsString()
		}
	}

	if inst := attrs[semconv.ServiceInstanceIDKey]; isUnexpandedSubstitution(inst) {
		t.Fatalf("after SDK merge, service.instance.id is a literal placeholder: %q", inst)
	}
	if inst := attrs[semconv.ServiceInstanceIDKey]; inst != "dev2meshstream--0000008-5cfcf66475-kztdt" {
		t.Fatalf("service.instance.id = %q, want pod name", inst)
	}
	if svc := attrs[semconv.ServiceNameKey]; svc != "mesh-stream" {
		t.Fatalf("service.name = %q, want mesh-stream", svc)
	}
}

// TestSDKMergeAllProvidersConsistent verifies traces, metrics, and logs all get the same
// correct service.instance.id after the SDK merge (the same resource is used for all three).
func TestSDKMergeAllProvidersConsistent(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES",
		"service.instance.id=$(CONTAINER_APP_REPLICA_NAME)")
	t.Setenv("OTEL_SERVICE_NAME", "mesh-stream")
	t.Setenv(envContainerAppReplica, "$(CONTAINER_APP_REPLICA_NAME)")
	t.Setenv(envPodName, "real-pod-name")

	res := telemetryResource("mesh-stream", "dev1", "1.0.0")
	merged := simulateSDKResourceMerge(t, res)

	var inst string
	for _, kv := range merged.Attributes() {
		if kv.Key == semconv.ServiceInstanceIDKey {
			inst = kv.Value.AsString()
		}
	}
	if inst != "real-pod-name" {
		t.Fatalf("service.instance.id = %q, want real-pod-name", inst)
	}
}

func TestSDKMergeSanitizedEnvRemovesHostNamespace(t *testing.T) {
	t.Setenv(EnvOTELResourceAttributes, "host.name=bad-host,host.arch=amd64,keep=yes")
	sanitizeAuthoritativeTelemetryEnvironment()
	res := telemetryResource("mesh-stream", "dev1", "1.0.0")
	merged := simulateSDKResourceMerge(t, res)

	for _, kv := range merged.Attributes() {
		k := string(kv.Key)
		if k == "host" || k == "hostname" || strings.HasPrefix(k, "host.") || k == "datadog.host.name" {
			t.Fatalf("expected sanitized SDK merge to omit host attrs, got %q", k)
		}
	}
}

func TestSDKMergeSanitizedEnvKeepsServiceNameAuthoritative(t *testing.T) {
	t.Setenv(EnvOTELResourceAttributes, "service.name=wrong-service,keep=yes")
	t.Setenv(EnvOTELServiceName, "mesh-stream")
	sanitizeAuthoritativeTelemetryEnvironment()
	res := telemetryResource("mesh-stream", "dev1", "1.0.0")
	merged := simulateSDKResourceMerge(t, res)

	attrs := make(map[attribute.Key]string)
	for _, kv := range merged.Attributes() {
		if kv.Value.Type() == attribute.STRING {
			attrs[kv.Key] = kv.Value.AsString()
		}
	}
	if svc := attrs[semconv.ServiceNameKey]; svc != "mesh-stream" {
		t.Fatalf("service.name = %q, want mesh-stream", svc)
	}
	if keep := attrs[attribute.Key("keep")]; keep != "yes" {
		t.Fatalf("keep = %q, want yes", keep)
	}
}

func TestSkipResourceKeyOnLogRecords(t *testing.T) {
	tests := []struct {
		key  string
		skip bool
	}{
		{"service", true},
		{"service.name", true},
		{"process.pid", true},
		{"telemetry.sdk.version", true},
		{"os.name", true},
		{"deployment.environment", true},
		{"deployment.environment.name", false},
		{"env", false},
		{"k8s.pod.name", false},
		{"host.name", true},
	}
	for _, tt := range tests {
		if got := skipResourceKeyOnLogRecords(tt.key); got != tt.skip {
			t.Errorf("skipResourceKeyOnLogRecords(%q) = %v, want %v", tt.key, got, tt.skip)
		}
	}
}

func recordAttrs(r slog.Record) map[string]string {
	attrs := make(map[string]string)
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	return attrs
}

func groupAttrs(a slog.Attr) map[string]string {
	attrs := make(map[string]string)
	for _, inner := range a.Value.Group() {
		attrs[inner.Key] = inner.Value.String()
	}
	return attrs
}
