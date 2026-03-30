package telemetry

import (
	"encoding"
	"fmt"
	"log/slog"
	"reflect"
)

const circularLogValue = "[circular]"

type normalizeVisit struct {
	typ  reflect.Type
	kind reflect.Kind
	ptr  uintptr
	len  int
	cap  int
}

func normalizeSlogValue(v slog.Value) slog.Value {
	v = v.Resolve()
	if v.Kind() != slog.KindAny {
		return v
	}

	normalized, changed := normalizeAnyForStructuredLog(v.Any())
	if !changed {
		return v
	}
	if text, ok := normalized.(string); ok {
		return slog.StringValue(text)
	}
	return slog.AnyValue(normalized)
}

func normalizeAnyForStructuredLog(v any) (any, bool) {
	return normalizeAnyForStructuredLogWithState(v, make(map[normalizeVisit]struct{}))
}

func normalizeAnyForStructuredLogWithState(v any, seen map[normalizeVisit]struct{}) (any, bool) {
	if v == nil {
		return nil, false
	}
	if text, ok := marshalTextLogValue(v); ok {
		return text, true
	}

	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Interface:
		if rv.IsNil() {
			return v, false
		}
		return normalizeAnyForStructuredLogWithState(rv.Elem().Interface(), seen)
	case reflect.Pointer:
		if rv.IsNil() {
			return v, false
		}
		visit, ok := makeNormalizeVisit(rv)
		if ok {
			if _, exists := seen[visit]; exists {
				return circularLogValue, true
			}
			seen[visit] = struct{}{}
			defer delete(seen, visit)
		}
		normalized, changed := normalizeAnyForStructuredLogWithState(rv.Elem().Interface(), seen)
		if !changed {
			return v, false
		}
		return normalized, true
	case reflect.Slice, reflect.Array:
		visit, ok := makeNormalizeVisit(rv)
		if ok {
			if _, exists := seen[visit]; exists {
				return circularLogValue, true
			}
			seen[visit] = struct{}{}
			defer delete(seen, visit)
		}
		items := make([]any, rv.Len())
		changed := false
		for i := 0; i < rv.Len(); i++ {
			item := rv.Index(i).Interface()
			normalized, itemChanged := normalizeAnyForStructuredLogWithState(item, seen)
			items[i] = normalized
			changed = changed || itemChanged
		}
		if !changed {
			return v, false
		}
		return items, true
	case reflect.Map:
		visit, ok := makeNormalizeVisit(rv)
		if ok {
			if _, exists := seen[visit]; exists {
				return circularLogValue, true
			}
			seen[visit] = struct{}{}
			defer delete(seen, visit)
		}

		if rv.Type().Key().Kind() == reflect.String {
			out := make(map[string]any, rv.Len())
			changed := false
			iter := rv.MapRange()
			for iter.Next() {
				key := iter.Key().String()
				item := iter.Value().Interface()
				normalized, itemChanged := normalizeAnyForStructuredLogWithState(item, seen)
				out[key] = normalized
				changed = changed || itemChanged
			}
			if !changed {
				return v, false
			}
			return out, true
		}

		if rv.Len() == 0 {
			return v, false
		}

		out := make(map[string]any, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			key := normalizeStructuredLogMapKey(iter.Key())
			item := iter.Value().Interface()
			normalized, _ := normalizeAnyForStructuredLogWithState(item, seen)
			out[key] = normalized
		}
		return out, true
	default:
		return v, false
	}
}

func makeNormalizeVisit(rv reflect.Value) (normalizeVisit, bool) {
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map:
		ptr := rv.Pointer()
		if ptr == 0 {
			return normalizeVisit{}, false
		}
		return normalizeVisit{typ: rv.Type(), kind: rv.Kind(), ptr: ptr}, true
	case reflect.Slice:
		if rv.IsNil() || rv.Len() == 0 {
			return normalizeVisit{}, false
		}
		return normalizeVisit{typ: rv.Type(), kind: rv.Kind(), ptr: rv.Pointer(), len: rv.Len(), cap: rv.Cap()}, true
	default:
		return normalizeVisit{}, false
	}
}

func normalizeStructuredLogMapKey(v reflect.Value) string {
	if !v.IsValid() {
		return ""
	}
	if text, ok := marshalTextLogValue(v.Interface()); ok {
		return text
	}
	if stringer, ok := v.Interface().(fmt.Stringer); ok {
		return stringer.String()
	}
	return fmt.Sprint(v.Interface())
}

func marshalTextLogValue(v any) (string, bool) {
	marshaler, ok := v.(encoding.TextMarshaler)
	if !ok {
		return "", false
	}
	text, err := marshaler.MarshalText()
	if err != nil {
		return "", false
	}
	return string(text), true
}
