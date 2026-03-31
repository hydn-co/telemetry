package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRecordStripForbiddenHostAttrs(t *testing.T) {
	r := slog.NewRecord(time.Time{}, slog.LevelInfo, "x", 0)
	r.AddAttrs(slog.String("host", "bad"), slog.String("ok", "yes"))
	out := recordStripReservedDatadogAttrs(nil, r)
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
	out := recordStripReservedDatadogAttrs(nil, r)
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
	if !foundHost {
		t.Fatal("expected nested g.host to be preserved")
	}
	if !foundKeep {
		t.Fatal("expected keep inside group")
	}
}

func TestCorrelationHandlerStripsHostFromRecord(t *testing.T) {
	next := &captureHandler{}
	logger := slog.New(&correlationHandler{
		next:                  next,
		resourceAttrs:         nil,
		serviceName:           "svc",
		deploymentEnvironment: "te",
		serviceVersion:        "tv",
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
	if attrs["dd.service"] != "svc" || attrs["dd.env"] != "te" || attrs["dd.version"] != "tv" {
		t.Fatalf("expected dd.* unified tags, got dd.service=%q dd.env=%q dd.version=%q",
			attrs["dd.service"], attrs["dd.env"], attrs["dd.version"])
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
	c := datadogHostStripReplaceAttr([]string{"peer"}, slog.String("host", "z"))
	if c.Key != "host" {
		t.Fatalf("expected nested peer.host kept, got %q", c.Key)
	}
}

func TestCorrelationHandlerStripsConflictingServiceAttrs(t *testing.T) {
	next := &captureHandler{}
	logger := slog.New(&correlationHandler{
		next:                  next,
		resourceAttrs:         nil,
		serviceName:           "svc",
		deploymentEnvironment: "te",
		serviceVersion:        "tv",
	})
	r := slog.NewRecord(time.Time{}, slog.LevelInfo, "hi", 0)
	r.AddAttrs(
		slog.Any("service", map[string]any{"name": "wrong"}),
		slog.String("service.name", "wrong"),
		slog.String("k", "v"),
	)
	_ = logger.Handler().Handle(context.Background(), r)
	if len(next.records) != 1 {
		t.Fatalf("got %d records", len(next.records))
	}
	attrs := recordAttrs(next.records[0])
	if attrs["service"] != "svc" {
		t.Fatalf("expected canonical service attr, got %q", attrs["service"])
	}
	if attrs["service.name"] != "" {
		t.Fatalf("service.name should be stripped, got %q", attrs["service.name"])
	}
	if attrs["k"] != "v" {
		t.Fatalf("expected k=v, got %q", attrs["k"])
	}
}

func TestCorrelationHandlerStripsConflictingDDUnifiedAttrs(t *testing.T) {
	next := &captureHandler{}
	logger := slog.New(&correlationHandler{
		next:                  next,
		resourceAttrs:         nil,
		serviceName:           "svc",
		deploymentEnvironment: "te",
		serviceVersion:        "tv",
	})
	r := slog.NewRecord(time.Time{}, slog.LevelInfo, "hi", 0)
	r.AddAttrs(
		slog.String("dd.service", "wrong-svc"),
		slog.String("dd.env", "wrong-env"),
		slog.String("dd.version", "wrong-ver"),
		slog.String("k", "v"),
	)
	_ = logger.Handler().Handle(context.Background(), r)
	attrs := recordAttrs(next.records[0])
	if attrs["dd.service"] != "svc" || attrs["dd.env"] != "te" || attrs["dd.version"] != "tv" {
		t.Fatalf("expected canonical dd.* tags, got dd.service=%q dd.env=%q dd.version=%q",
			attrs["dd.service"], attrs["dd.env"], attrs["dd.version"])
	}
	if attrs["k"] != "v" {
		t.Fatalf("expected k=v, got %q", attrs["k"])
	}
}

func TestCorrelationHandlerNormalizesUUIDAttrToString(t *testing.T) {
	next := &captureHandler{}
	logger := slog.New(&correlationHandler{
		next:                  next,
		resourceAttrs:         nil,
		serviceName:           "svc",
		deploymentEnvironment: "te",
		serviceVersion:        "tv",
	})
	id := uuid.MustParse("01234567-89ab-cdef-0123-456789abcdef")

	logger.Info("hi", "id", id)

	if len(next.records) != 1 {
		t.Fatalf("got %d records", len(next.records))
	}

	var got slog.Attr
	next.records[0].Attrs(func(a slog.Attr) bool {
		if a.Key == "id" {
			got = a
			return false
		}
		return true
	})
	if got.Key == "" {
		t.Fatal("expected id attr")
	}
	if got.Value.Kind() != slog.KindString {
		t.Fatalf("expected id attr to be string, got %v", got.Value.Kind())
	}
	if got.Value.String() != id.String() {
		t.Fatalf("expected id %q, got %q", id.String(), got.Value.String())
	}
}

func TestCorrelationHandlerNormalizesUUIDsInsideMapsAndSlices(t *testing.T) {
	next := &captureHandler{}
	logger := slog.New(&correlationHandler{
		next:                  next,
		resourceAttrs:         nil,
		serviceName:           "svc",
		deploymentEnvironment: "te",
		serviceVersion:        "tv",
	})
	id := uuid.MustParse("01234567-89ab-cdef-0123-456789abcdef")
	payload := map[string]any{
		"id":  id,
		"ids": []uuid.UUID{id},
	}

	logger.Info("hi", "payload", payload)

	if len(next.records) != 1 {
		t.Fatalf("got %d records", len(next.records))
	}

	var got slog.Attr
	next.records[0].Attrs(func(a slog.Attr) bool {
		if a.Key == "payload" {
			got = a
			return false
		}
		return true
	})
	if got.Key == "" {
		t.Fatal("expected payload attr")
	}
	payloadValue, ok := got.Value.Any().(map[string]any)
	if !ok {
		t.Fatalf("expected normalized payload map, got %T", got.Value.Any())
	}
	if payloadValue["id"] != id.String() {
		t.Fatalf("expected payload id %q, got %#v", id.String(), payloadValue["id"])
	}
	ids, ok := payloadValue["ids"].([]any)
	if !ok {
		t.Fatalf("expected normalized ids slice, got %T", payloadValue["ids"])
	}
	if len(ids) != 1 || ids[0] != id.String() {
		t.Fatalf("expected ids [%q], got %#v", id.String(), ids)
	}
}

func TestCorrelationHandlerWithAttrsStripsConflictingServiceAttrs(t *testing.T) {
	var buf bytes.Buffer
	jsonH := slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: datadogHostStripReplaceAttr})
	logger := slog.New(&correlationHandler{
		next:                  jsonH,
		resourceAttrs:         nil,
		serviceName:           "svc",
		deploymentEnvironment: "te",
		serviceVersion:        "tv",
	}).With("service", map[string]any{"name": "wrong"}, "service.name", "wrong", "a", "1")

	logger.Info("m")

	line := strings.TrimSpace(buf.String())
	if strings.Contains(line, `"service":{"name":"wrong"}`) {
		t.Fatalf("service blob must be stripped: %s", line)
	}
	if strings.Contains(line, `"service.name"`) {
		t.Fatalf("service.name must be stripped: %s", line)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["service"] != "svc" || obj["dd.service"] != "svc" || obj["dd.env"] != "te" || obj["dd.version"] != "tv" {
		t.Fatalf("expected canonical service and dd.* tags, got %#v", obj)
	}
	if obj["a"] != "1" {
		t.Fatalf("expected a=1, got %#v", obj["a"])
	}
}

func TestCorrelationHandlerWithGroupStripsTopLevelServiceGroup(t *testing.T) {
	var buf bytes.Buffer
	jsonH := slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: datadogHostStripReplaceAttr})
	logger := slog.New(&correlationHandler{
		next:                  jsonH,
		serviceName:           "svc",
		deploymentEnvironment: "te",
		serviceVersion:        "tv",
	}).WithGroup("service").With("name", "wrong")

	logger.Info("m")

	line := strings.TrimSpace(buf.String())
	if strings.Contains(line, `"service":{"name":"wrong"`) {
		t.Fatalf("service group must be stripped: %s", line)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["service"] != "svc" || obj["dd.service"] != "svc" {
		t.Fatalf("expected canonical top-level service and dd.service, got %#v", obj)
	}
	if _, ok := obj["name"]; ok {
		t.Fatalf("service group attrs must not leak to top level: %s", line)
	}
}

func TestCorrelationHandlerWithGroupStripsTopLevelHostGroup(t *testing.T) {
	var buf bytes.Buffer
	jsonH := slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: datadogHostStripReplaceAttr})
	logger := slog.New(&correlationHandler{
		next:                  jsonH,
		serviceName:           "svc",
		deploymentEnvironment: "te",
		serviceVersion:        "tv",
	}).WithGroup("host").With("name", "bad-host")

	logger.Info("m")

	line := strings.TrimSpace(buf.String())
	if strings.Contains(line, `"host"`) {
		t.Fatalf("host group must be stripped: %s", line)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["service"] != "svc" || obj["dd.service"] != "svc" {
		t.Fatalf("expected canonical top-level service and dd.service, got %#v", obj)
	}
}

func TestCorrelationHandlerPreservesNestedPeerServiceAndHost(t *testing.T) {
	var buf bytes.Buffer
	jsonH := slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: datadogHostStripReplaceAttr})
	logger := slog.New(&correlationHandler{
		next:                  jsonH,
		serviceName:           "svc",
		deploymentEnvironment: "te",
		serviceVersion:        "tv",
	}).WithGroup("peer").With("service", "postgres", "host", "db.internal")

	logger.Info("m")

	line := strings.TrimSpace(buf.String())
	var obj map[string]any
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["service"] != "svc" || obj["dd.service"] != "svc" {
		t.Fatalf("expected canonical top-level service and dd.service, got %#v", obj)
	}
	peer, ok := obj["peer"].(map[string]any)
	if !ok {
		t.Fatalf("expected peer group, got %#v", obj["peer"])
	}
	if peer["service"] != "postgres" {
		t.Fatalf("expected nested peer.service preserved, got %#v", peer["service"])
	}
	if peer["host"] != "db.internal" {
		t.Fatalf("expected nested peer.host preserved, got %#v", peer["host"])
	}
}

func TestCorrelationHandlerMergesRepeatedWithAttrsUnderSameGroup(t *testing.T) {
	var buf bytes.Buffer
	jsonH := slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: datadogHostStripReplaceAttr})
	logger := slog.New(&correlationHandler{
		next:                  jsonH,
		serviceName:           "svc",
		deploymentEnvironment: "te",
		serviceVersion:        "tv",
	}).WithGroup("peer").With("service", "postgres").With("host", "db.internal")

	logger.Info("m", "role", "read")

	var obj map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &obj); err != nil {
		t.Fatal(err)
	}
	peer, ok := obj["peer"].(map[string]any)
	if !ok {
		t.Fatalf("expected peer group, got %#v", obj["peer"])
	}
	if peer["service"] != "postgres" || peer["host"] != "db.internal" || peer["role"] != "read" {
		t.Fatalf("expected merged peer attrs, got %#v", peer)
	}
	if obj["service"] != "svc" || obj["dd.service"] != "svc" {
		t.Fatalf("expected canonical top-level service and dd.service, got %#v", obj)
	}
}

func TestFilterForbiddenHostAttrs(t *testing.T) {
	filtered := filterReservedDatadogLogAttrs(nil, []slog.Attr{
		slog.String("host", "x"),
		slog.String("service", "blob"),
		slog.String("dd.service", "blob-dd"),
		slog.String("peer.service", "postgres"),
		slog.Int("n", 1),
	}, false)
	if len(filtered) != 2 || filtered[0].Key != "peer.service" || filtered[1].Key != "n" {
		t.Fatalf("got %+v", filtered)
	}
}

func TestStripDatadogHostHandlerEndToEndJSON(t *testing.T) {
	var buf bytes.Buffer
	jsonH := slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: datadogHostStripReplaceAttr})
	h := &stripReservedDatadogAttrsHandler{next: jsonH}
	h2 := h.WithAttrs([]slog.Attr{slog.String("host", "bad"), slog.String("service", "blob"), slog.String("dd.service", "blob"), slog.String("a", "1")})
	_ = h2.Handle(context.Background(), slog.NewRecord(time.Time{}, slog.LevelInfo, "m", 0))
	line := buf.String()
	if strings.Contains(line, `"host"`) {
		t.Fatalf("JSON must not contain host key: %s", line)
	}
	if strings.Contains(line, `"service"`) {
		t.Fatalf("JSON must not contain reserved service key: %s", line)
	}
	if strings.Contains(line, `"dd.service"`) {
		t.Fatalf("JSON must not contain reserved dd.service from WithAttrs: %s", line)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["a"] != "1" {
		t.Fatalf("expected a=1, got %v", obj["a"])
	}
}

func TestStripHandlerPreservesFlatServiceForOTLPBridge(t *testing.T) {
	// After correlationHandler, records carry a flat string "service" for Datadog.
	// The OTLP path must not strip it; resource service.name alone is not always
	// mapped to Log Explorer's Service facet (e.g. ACA managed OTLP → Datadog).
	var buf bytes.Buffer
	jsonH := slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: datadogHostStripReplaceAttr})
	h := &stripReservedDatadogAttrsHandler{next: jsonH}
	r := slog.NewRecord(time.Time{}, slog.LevelInfo, "m", 0)
	r.AddAttrs(
		slog.String("service", "control-api"),
		slog.String("dd.service", "control-api"),
		slog.String("dd.env", "prod"),
		slog.String("dd.version", "1.0.0"),
		slog.String("msg_key", "v"),
	)
	_ = h.Handle(context.Background(), r)
	line := strings.TrimSpace(buf.String())
	for _, want := range []string{`"service":"control-api"`, `"dd.service":"control-api"`, `"dd.env":"prod"`, `"dd.version":"1.0.0"`} {
		if !strings.Contains(line, want) {
			t.Fatalf("expected %q in OTLP bridge output, got %s", want, line)
		}
	}
	if !strings.Contains(line, `"msg_key":"v"`) {
		t.Fatalf("expected msg_key preserved, got %s", line)
	}
}

func TestStripReservedDatadogAttrsHandlerWithGroupStripsTopLevelService(t *testing.T) {
	var buf bytes.Buffer
	jsonH := slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: datadogHostStripReplaceAttr})
	h := &stripReservedDatadogAttrsHandler{next: jsonH}
	h2 := h.WithGroup("service").WithAttrs([]slog.Attr{slog.String("name", "wrong")})
	_ = h2.Handle(context.Background(), slog.NewRecord(time.Time{}, slog.LevelInfo, "m", 0))
	line := strings.TrimSpace(buf.String())
	if strings.Contains(line, `"service"`) {
		t.Fatalf("top-level service group must be stripped: %s", line)
	}
}
