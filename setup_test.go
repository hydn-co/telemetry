package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace"
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
	t.Setenv(EnvOTELExporterOTLPEndpoint, "http://127.0.0.1:4317")
	t.Setenv(EnvOTELServiceName, "test-svc")
	t.Setenv(EnvOTELDeploymentEnvironment, "test")
	t.Setenv(EnvOTELServiceVersion, "1.0.0")

	shutdown := Setup(context.Background(), Options{})
	shutdown()
	shutdown()
}

func TestCorrelationHandler_injectsServiceEnvVersion(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	h := &correlationHandler{next: base, serviceName: "svc1", env: "dev1", version: "1.0.0"}
	logger := slog.New(h)
	logger.Info("msg")
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if m["service"] != "svc1" || m["env"] != "dev1" || m["version"] != "1.0.0" {
		t.Errorf("missing or wrong correlation fields: %+v", m)
	}
	if m["trace_id"] != nil || m["span_id"] != nil {
		t.Errorf("should not have trace_id/span_id without span: %+v", m)
	}
}

func TestCorrelationHandler_injectsTraceIDAndSpanIDWhenSpanInContext(t *testing.T) {
	tp := trace.NewTracerProvider()
	defer func() { _ = tp.Shutdown(context.Background()) }()
	ctx, span := tp.Tracer("test").Start(context.Background(), "test")
	defer span.End()

	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	h := &correlationHandler{next: base, serviceName: "svc", env: "env", version: "v1"}
	logger := slog.New(h)
	logger.InfoContext(ctx, "msg")
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	traceID, _ := m["trace_id"].(string)
	spanID, _ := m["span_id"].(string)
	if traceID == "" || spanID == "" {
		t.Errorf("missing trace_id or span_id: %+v", m)
	}
	if ok, _ := regexp.MatchString(`^[0-9a-f]{32}$`, traceID); !ok {
		t.Errorf("trace_id should be 32-char hex: %q", traceID)
	}
	if ok, _ := regexp.MatchString(`^[0-9a-f]{16}$`, spanID); !ok {
		t.Errorf("span_id should be 16-char hex: %q", spanID)
	}
}

func TestSetup_installsJSONHandlerWithCorrelation(t *testing.T) {
	t.Setenv(EnvOTELExporterOTLPEndpoint, "http://127.0.0.1:4317")
	t.Setenv(EnvOTELServiceName, "installed-svc")
	t.Setenv(EnvOTELDeploymentEnvironment, "testenv")
	t.Setenv(EnvOTELServiceVersion, "2.0.0")
	t.Setenv(EnvLogFile, "") // no file

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	shutdown := Setup(context.Background(), Options{})
	slog.Info("test line")
	_ = w.Close()
	os.Stdout = oldStdout
	var out bytes.Buffer
	_, _ = io.Copy(&out, r)
	shutdown()

	// stdout has one JSON object per line; take the line with "test line"
	lines := bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n"))
	var line []byte
	for _, l := range lines {
		if bytes.Contains(l, []byte("test line")) {
			line = l
			break
		}
	}
	if line == nil {
		t.Fatalf("no line contained 'test line': %s", out.Bytes())
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if m["service"] != "installed-svc" || m["env"] != "testenv" || m["version"] != "2.0.0" {
		t.Errorf("correlation fields missing or wrong: %+v", m)
	}
}

func TestSetup_LOGFileWritesCorrelationFields(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")
	t.Setenv(EnvOTELExporterOTLPEndpoint, "http://127.0.0.1:4317")
	t.Setenv(EnvOTELServiceName, "file-svc")
	t.Setenv(EnvOTELDeploymentEnvironment, "prod")
	t.Setenv(EnvOTELServiceVersion, "3.0.0")
	t.Setenv(EnvLogFile, logPath)

	shutdown := Setup(context.Background(), Options{})
	slog.Info("file test line")
	shutdown()

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	// Log file has one JSON object per line; find the "file test line" record
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	var line []byte
	for _, l := range lines {
		if bytes.Contains(l, []byte("file test line")) {
			line = l
			break
		}
	}
	if line == nil {
		t.Fatalf("no line contained 'file test line': %s", data)
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("invalid JSON in log file: %v", err)
	}
	if m["service"] != "file-svc" || m["env"] != "prod" || m["version"] != "3.0.0" {
		t.Errorf("file log missing correlation: %+v", m)
	}
}

func TestSetup_ShutdownClosesLogFile(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "close.log")
	t.Setenv(EnvOTELExporterOTLPEndpoint, "http://127.0.0.1:4317")
	t.Setenv(EnvOTELServiceName, "svc")
	t.Setenv(EnvOTELDeploymentEnvironment, "e")
	t.Setenv(EnvOTELServiceVersion, "1")
	t.Setenv(EnvLogFile, logPath)

	shutdown := Setup(context.Background(), Options{})
	slog.Info("before shutdown")
	shutdown()
	shutdown() // safe to call again

	// After shutdown the file should be closed; we can open again and append (new handle)
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Error("log file should contain at least one line")
	}
}
