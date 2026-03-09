package telemetry

import (
	"context"
	"testing"
)

func TestSetup_PanicsWhenEnvMissing(t *testing.T) {
	tests := []struct {
		name      string
		setEnv    func(*testing.T)
		wantPanic bool
	}{
		{
			name: "missing all",
			setEnv: func(t *testing.T) {
				t.Setenv(EnvOTELExporterOTLPEndpoint, "")
				t.Setenv(EnvOTELServiceName, "")
				t.Setenv(EnvOTELDeploymentEnvironment, "")
				t.Setenv(EnvOTELServiceVersion, "")
			},
			wantPanic: true,
		},
		{
			name: "missing endpoint",
			setEnv: func(t *testing.T) {
				t.Setenv(EnvOTELExporterOTLPEndpoint, "")
				t.Setenv(EnvOTELServiceName, "svc")
				t.Setenv(EnvOTELDeploymentEnvironment, "dev1")
				t.Setenv(EnvOTELServiceVersion, "1.0.0")
			},
			wantPanic: true,
		},
		{
			name: "missing service name",
			setEnv: func(t *testing.T) {
				t.Setenv(EnvOTELExporterOTLPEndpoint, "http://127.0.0.1:4317")
				t.Setenv(EnvOTELServiceName, "")
				t.Setenv(EnvOTELDeploymentEnvironment, "dev1")
				t.Setenv(EnvOTELServiceVersion, "1.0.0")
			},
			wantPanic: true,
		},
		{
			name: "missing deployment environment",
			setEnv: func(t *testing.T) {
				t.Setenv(EnvOTELExporterOTLPEndpoint, "http://127.0.0.1:4317")
				t.Setenv(EnvOTELServiceName, "svc")
				t.Setenv(EnvOTELDeploymentEnvironment, "")
				t.Setenv(EnvOTELServiceVersion, "1.0.0")
			},
			wantPanic: true,
		},
		{
			name: "missing service version",
			setEnv: func(t *testing.T) {
				t.Setenv(EnvOTELExporterOTLPEndpoint, "http://127.0.0.1:4317")
				t.Setenv(EnvOTELServiceName, "svc")
				t.Setenv(EnvOTELDeploymentEnvironment, "dev1")
				t.Setenv(EnvOTELServiceVersion, "")
			},
			wantPanic: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setEnv(t)
			var panicked bool
			func() {
				defer func() {
					if recover() != nil {
						panicked = true
					}
				}()
				Setup(context.Background(), Options{})
			}()
			if !panicked {
				t.Error("Setup should panic when required env vars are missing")
			}
		})
	}
}

func TestSetup_WithAllEnvSet(t *testing.T) {
	// All required env set; exporter may fail to connect but we get past requireOTELEnv.
	// If New succeeds we get a shutdown func; if it fails we get a no-op. Either way no panic.
	t.Setenv(EnvOTELExporterOTLPEndpoint, "http://127.0.0.1:4317")
	t.Setenv(EnvOTELServiceName, "test-svc")
	t.Setenv(EnvOTELDeploymentEnvironment, "test")
	t.Setenv(EnvOTELServiceVersion, "1.0.0")

	shutdown := Setup(context.Background(), Options{})
	// Shutdown is safe to call multiple times
	shutdown()
	shutdown()
}
