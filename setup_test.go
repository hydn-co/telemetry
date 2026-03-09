package telemetry

import (
	"context"
	"testing"
)

func TestEnabled(t *testing.T) {
	t.Run("false when env unset", func(t *testing.T) {
		t.Setenv(EnvOTELExporterOTLPEndpoint, "")
		t.Setenv(EnvOTELServiceName, "")
		if Enabled() {
			t.Error("Enabled() should be false when both env vars are unset")
		}
	})

	t.Run("true when OTEL_EXPORTER_OTLP_ENDPOINT set", func(t *testing.T) {
		t.Setenv(EnvOTELExporterOTLPEndpoint, "http://127.0.0.1:4317")
		t.Setenv(EnvOTELServiceName, "")
		if !Enabled() {
			t.Error("Enabled() should be true when OTEL_EXPORTER_OTLP_ENDPOINT is set")
		}
	})

	t.Run("true when OTEL_SERVICE_NAME set", func(t *testing.T) {
		t.Setenv(EnvOTELExporterOTLPEndpoint, "")
		t.Setenv(EnvOTELServiceName, "my-service")
		if !Enabled() {
			t.Error("Enabled() should be true when OTEL_SERVICE_NAME is set")
		}
	})
}

func TestSetup_WhenDisabled(t *testing.T) {
	t.Setenv(EnvOTELExporterOTLPEndpoint, "")
	t.Setenv(EnvOTELServiceName, "")

	ctx := context.Background()
	shutdown := Setup(ctx, Options{ServiceName: "test-svc", Version: "1.0.0"})

	// Shutdown should be no-op and safe to call multiple times
	shutdown()
	shutdown()
}

func TestSetup_ShutdownIdempotent(t *testing.T) {
	// When OTel is disabled, returned shutdown is no-op; multiple calls must not panic
	t.Setenv(EnvOTELExporterOTLPEndpoint, "")
	t.Setenv(EnvOTELServiceName, "")

	shutdown := Setup(context.Background(), Options{ServiceName: "idempotent-test"})
	for i := 0; i < 3; i++ {
		shutdown()
	}
}
