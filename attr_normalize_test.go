package telemetry

import (
	"testing"

	"github.com/google/uuid"
)

func TestNormalizeAnyForStructuredLogHandlesMapCycle(t *testing.T) {
	id := uuid.MustParse("01234567-89ab-cdef-0123-456789abcdef")
	payload := map[string]any{}
	payload["id"] = id
	payload["self"] = payload

	normalized, changed := normalizeAnyForStructuredLog(payload)
	if !changed {
		t.Fatal("expected cyclic payload to be normalized")
	}

	normalizedMap, ok := normalized.(map[string]any)
	if !ok {
		t.Fatalf("expected normalized map, got %T", normalized)
	}
	if normalizedMap["id"] != id.String() {
		t.Fatalf("expected normalized id %q, got %#v", id.String(), normalizedMap["id"])
	}
	if normalizedMap["self"] != circularLogValue {
		t.Fatalf("expected cycle marker %q, got %#v", circularLogValue, normalizedMap["self"])
	}
}

func TestNormalizeAnyForStructuredLogHandlesSliceCycle(t *testing.T) {
	payload := make([]any, 1)
	payload[0] = payload

	normalized, changed := normalizeAnyForStructuredLog(payload)
	if !changed {
		t.Fatal("expected cyclic slice to be normalized")
	}

	normalizedSlice, ok := normalized.([]any)
	if !ok {
		t.Fatalf("expected normalized slice, got %T", normalized)
	}
	if len(normalizedSlice) != 1 || normalizedSlice[0] != circularLogValue {
		t.Fatalf("expected [%q], got %#v", circularLogValue, normalizedSlice)
	}
}

func TestNormalizeAnyForStructuredLogNormalizesIntKeyMapValues(t *testing.T) {
	id := uuid.MustParse("01234567-89ab-cdef-0123-456789abcdef")
	payload := map[int]uuid.UUID{42: id}

	normalized, changed := normalizeAnyForStructuredLog(payload)
	if !changed {
		t.Fatal("expected non-string-key map to be normalized")
	}

	normalizedMap, ok := normalized.(map[string]any)
	if !ok {
		t.Fatalf("expected normalized map, got %T", normalized)
	}
	if len(normalizedMap) != 1 || normalizedMap["42"] != id.String() {
		t.Fatalf("expected map[\"42\"]=%q, got %#v", id.String(), normalizedMap)
	}
}

func TestNormalizeAnyForStructuredLogNormalizesUUIDKeyMap(t *testing.T) {
	id := uuid.MustParse("01234567-89ab-cdef-0123-456789abcdef")
	payload := map[uuid.UUID]string{id: "ok"}

	normalized, changed := normalizeAnyForStructuredLog(payload)
	if !changed {
		t.Fatal("expected UUID-keyed map to be normalized")
	}

	normalizedMap, ok := normalized.(map[string]any)
	if !ok {
		t.Fatalf("expected normalized map, got %T", normalized)
	}
	if len(normalizedMap) != 1 || normalizedMap[id.String()] != "ok" {
		t.Fatalf("expected map[%q]=ok, got %#v", id.String(), normalizedMap)
	}
}

func TestNormalizeAnyForStructuredLogKeepsSharedMapsWithoutCycleMarker(t *testing.T) {
	id := uuid.MustParse("01234567-89ab-cdef-0123-456789abcdef")
	shared := map[string]any{"id": id}
	payload := map[string]any{
		"first":  shared,
		"second": shared,
	}

	normalized, changed := normalizeAnyForStructuredLog(payload)
	if !changed {
		t.Fatal("expected shared payload to be normalized")
	}

	normalizedMap, ok := normalized.(map[string]any)
	if !ok {
		t.Fatalf("expected normalized map, got %T", normalized)
	}
	first, ok := normalizedMap["first"].(map[string]any)
	if !ok {
		t.Fatalf("expected first map, got %T", normalizedMap["first"])
	}
	second, ok := normalizedMap["second"].(map[string]any)
	if !ok {
		t.Fatalf("expected second map, got %T", normalizedMap["second"])
	}
	if first["id"] != id.String() || second["id"] != id.String() {
		t.Fatalf("expected shared map IDs to normalize, got first=%#v second=%#v", first, second)
	}
	if first["id"] == circularLogValue || second["id"] == circularLogValue {
		t.Fatalf("did not expect cycle marker in shared maps, got first=%#v second=%#v", first, second)
	}
}
