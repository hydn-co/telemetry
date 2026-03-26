package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestRecordStripForbiddenHostAttrs(t *testing.T) {
	r := slog.NewRecord(time.Time{}, slog.LevelInfo, "x", 0)
	r.AddAttrs(slog.String("host", "bad"), slog.String("ok", "yes"))
	out := recordStripForbiddenHostAttrs(r)
	attrs := recordAttrs(out)
	if attrs["host"] != "" {
		t.Fatalf("expected host stripped, got %q", attrs["host"])
	}
	if attrs["ok"] != "yes" {
		t.Fatalf("expected ok preserved, got %q", attrs["ok"])
	}
}

func TestRecordStripForbiddenHostAttrsNestedGroup(t *testing.T) {
	r := slog.NewRecord(time.Time{}, slog.LevelInfo, "x", 0)
	r.AddAttrs(slog.Group("g",
		slog.String("host", "inner"),
		slog.String("keep", "v"),
	))
	out := recordStripForbiddenHostAttrs(r)
	var foundHost, foundKeep bool
	out.Attrs(func(a slog.Attr) bool {
		if a.Key == "g" && a.Value.Kind() == slog.KindGroup {
			for _, inner := range a.Value.Group() {
				switch inner.Key {
				case "host":
					foundHost = true
				case "keep":
					foundKeep = true
				}
			}
		}
		return true
	})
	if foundHost {
		t.Fatal("expected nested host stripped")
	}
	if !foundKeep {
		t.Fatal("expected keep inside group")
	}
}

func TestCorrelationHandlerStripsHostFromRecord(t *testing.T) {
	next := &captureHandler{}
	logger := slog.New(&correlationHandler{
		next:          next,
		resourceAttrs: nil,
		serviceName:   "svc",
	})
	r := slog.NewRecord(time.Time{}, slog.LevelInfo, "hi", 0)
	r.AddAttrs(slog.String("host", "should-not-appear"), slog.String("k", "v"))
	_ = logger.Handler().Handle(context.Background(), r)
	if len(next.records) != 1 {
		t.Fatalf("got %d records", len(next.records))
	}
	attrs := recordAttrs(next.records[0])
	if attrs["host"] != "" {
		t.Fatalf("host should be stripped, got %q", attrs["host"])
	}
	if attrs["k"] != "v" {
		t.Fatalf("expected k=v, got %q", attrs["k"])
	}
}

func TestDatadogHostStripReplaceAttr(t *testing.T) {
	a := datadogHostStripReplaceAttr(nil, slog.String("host", "x"))
	if !a.Equal(slog.Attr{}) {
		t.Fatalf("expected empty attr, got %+v", a)
	}
	b := datadogHostStripReplaceAttr(nil, slog.String("other", "y"))
	if b.Key != "other" {
		t.Fatalf("expected other kept, got %q", b.Key)
	}
}

func TestFilterForbiddenHostAttrs(t *testing.T) {
	filtered := filterForbiddenHostAttrs([]slog.Attr{
		slog.String("host", "x"),
		slog.Int("n", 1),
	})
	if len(filtered) != 1 || filtered[0].Key != "n" {
		t.Fatalf("got %+v", filtered)
	}
}

func TestStripDatadogHostHandlerEndToEndJSON(t *testing.T) {
	var buf bytes.Buffer
	jsonH := slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: datadogHostStripReplaceAttr})
	h := &stripDatadogHostHandler{next: jsonH}
	h2 := h.WithAttrs([]slog.Attr{slog.String("host", "bad"), slog.String("a", "1")})
	_ = h2.Handle(context.Background(), slog.NewRecord(time.Time{}, slog.LevelInfo, "m", 0))
	line := buf.String()
	if strings.Contains(line, `"host"`) {
		t.Fatalf("JSON must not contain host key: %s", line)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["a"] != "1" {
		t.Fatalf("expected a=1, got %v", obj["a"])
	}
}
