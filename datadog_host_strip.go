package telemetry

import (
	"context"
	"log/slog"
	"strings"
)

func splitSlogPathComponent(component string) []string {
	component = strings.TrimSpace(component)
	if component == "" {
		return nil
	}
	parts := strings.Split(component, ".")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

func appendSlogPath(groups []string, key string) []string {
	var path []string
	for _, group := range groups {
		path = append(path, splitSlogPathComponent(group)...)
	}
	path = append(path, splitSlogPathComponent(key)...)
	return path
}

func isForbiddenDatadogHostAttrPath(path []string) bool {
	if len(path) == 0 {
		return false
	}
	if path[0] == "host" || path[0] == "hostname" {
		return true
	}
	return len(path) >= 3 && path[0] == "datadog" && path[1] == "host" && path[2] == "name"
}

// isDatadogUnifiedTagPath reports reserved dd.service / dd.env / dd.version paths (flat or nested).
func isDatadogUnifiedTagPath(path []string) bool {
	if len(path) < 2 || path[0] != "dd" {
		return false
	}
	switch path[1] {
	case "service", "env", "version":
		return true
	default:
		return false
	}
}

// shouldStripDatadogUnifiedTagAttr removes user-supplied dd.* unified tags; preserves them
// on the OTLP bridge path after correlationHandler (same rules as flat "service").
func shouldStripDatadogUnifiedTagAttr(path []string, v slog.Value, preserveAuthoritativeForOTLP bool) bool {
	if !isDatadogUnifiedTagPath(path) {
		return false
	}
	if v.Kind() == slog.KindGroup {
		return true
	}
	if preserveAuthoritativeForOTLP {
		return false
	}
	return true
}

// shouldStripServiceAttr reports whether a slog attribute under path should be removed.
// When preserveFlatServiceString is true (OTLP bridge path, after correlationHandler), a
// top-level string attribute "service" is kept so Datadog Log Explorer can populate the
// Service facet from OTLP log attributes; Azure ACA → Datadog does not always map
// resource service.name to that facet. User-supplied service (correlation input,
// strip handler WithAttrs) uses preserveFlatServiceString=false so callers cannot override.
func shouldStripServiceAttr(path []string, v slog.Value, preserveFlatServiceString bool) bool {
	if len(path) == 0 || path[0] != "service" {
		return false
	}
	if len(path) >= 2 {
		return true
	}
	// Top-level key "service"
	if v.Kind() == slog.KindGroup {
		return true
	}
	if preserveFlatServiceString {
		return false
	}
	return true
}

func appendRawGroup(groups []string, name string) []string {
	if name == "" {
		return append([]string(nil), groups...)
	}
	next := append([]string(nil), groups...)
	next = append(next, name)
	return next
}

type scopedSlogAttrs struct {
	groups []string
	attrs  []slog.Attr
}

func cloneScopedSlogAttrs(scoped []scopedSlogAttrs) []scopedSlogAttrs {
	if len(scoped) == 0 {
		return nil
	}
	cloned := make([]scopedSlogAttrs, 0, len(scoped))
	for _, scope := range scoped {
		cloned = append(cloned, scopedSlogAttrs{
			groups: append([]string(nil), scope.groups...),
			attrs:  append([]slog.Attr(nil), scope.attrs...),
		})
	}
	return cloned
}

func addAttrsAtGroupPath(dst *[]slog.Attr, groups []string, attrs []slog.Attr) {
	if len(attrs) == 0 {
		return
	}
	if len(groups) == 0 {
		*dst = append(*dst, attrs...)
		return
	}
	key := groups[0]
	for i, existing := range *dst {
		resolved := existing.Value.Resolve()
		if existing.Key != key || resolved.Kind() != slog.KindGroup {
			continue
		}
		inner := append([]slog.Attr(nil), resolved.Group()...)
		addAttrsAtGroupPath(&inner, groups[1:], attrs)
		(*dst)[i] = slog.Attr{Key: key, Value: slog.GroupValue(inner...)}
		return
	}
	inner := append([]slog.Attr(nil), attrs...)
	for i := len(groups) - 1; i >= 1; i-- {
		inner = []slog.Attr{{Key: groups[i], Value: slog.GroupValue(inner...)}}
	}
	*dst = append(*dst, slog.Attr{Key: key, Value: slog.GroupValue(inner...)})
}

func materializeScopedSlogAttrs(scoped []scopedSlogAttrs) []slog.Attr {
	var out []slog.Attr
	for _, scope := range scoped {
		addAttrsAtGroupPath(&out, scope.groups, scope.attrs)
	}
	return out
}

// datadogHostStripReplaceAttr is for [slog.HandlerOptions.ReplaceAttr]: drop top-level
// host-like keys so JSONHandler cannot emit Datadog host overrides.
func datadogHostStripReplaceAttr(groups []string, a slog.Attr) slog.Attr {
	if isForbiddenDatadogHostAttrPath(appendSlogPath(groups, a.Key)) {
		return slog.Attr{}
	}
	return a
}

// filterReservedDatadogLogAttrs returns a copy of attrs without reserved Datadog
// top-level host/service namespaces, recursively for [slog.KindGroup].
// When preserveFlatServiceString is true, authoritative top-level "service" and
// dd.service / dd.env / dd.version (from correlationHandler) are kept for OTLP export;
// user contexts pass false.
func filterReservedDatadogLogAttrs(groups []string, attrs []slog.Attr, preserveFlatServiceString bool) []slog.Attr {
	var out []slog.Attr
	for _, a := range attrs {
		a.Value = a.Value.Resolve()
		path := appendSlogPath(groups, a.Key)
		if isForbiddenDatadogHostAttrPath(path) {
			continue
		}
		if shouldStripDatadogUnifiedTagAttr(path, a.Value, preserveFlatServiceString) {
			continue
		}
		if shouldStripServiceAttr(path, a.Value, preserveFlatServiceString) {
			continue
		}
		if a.Value.Kind() == slog.KindGroup {
			inner := filterReservedDatadogLogAttrs(appendRawGroup(groups, a.Key), a.Value.Group(), preserveFlatServiceString)
			if len(inner) == 0 {
				continue
			}
			out = append(out, slog.Attr{Key: a.Key, Value: slog.GroupValue(inner...)})
			continue
		}
		out = append(out, slog.Attr{Key: a.Key, Value: normalizeSlogValue(a.Value)})
	}
	return out
}

// recordReservedDatadogAttrs returns the filtered attrs from r using the current slog
// group path for top-level Datadog authority checks. User records must pass
// preserveFlatServiceString=false so callers cannot override service via slog.
func recordReservedDatadogAttrs(groups []string, r slog.Record, preserveFlatServiceString bool) []slog.Attr {
	var attrs []slog.Attr
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a)
		return true
	})
	return filterReservedDatadogLogAttrs(groups, attrs, preserveFlatServiceString)
}

func recordStripReservedDatadogAttrs(groups []string, r slog.Record) slog.Record {
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	out.AddAttrs(materializeScopedSlogAttrs([]scopedSlogAttrs{{groups: append([]string(nil), groups...), attrs: recordReservedDatadogAttrs(groups, r, false)}})...)
	return out
}

// stripReservedDatadogAttrsHandler removes reserved Datadog attributes before
// delegating to the next handler (OTLP bridge). It filters With() attrs so they are
// never stored on the child.
type stripReservedDatadogAttrsHandler struct {
	next        slog.Handler
	scopedAttrs []scopedSlogAttrs
	groups      []string
}

func (h *stripReservedDatadogAttrsHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *stripReservedDatadogAttrsHandler) Handle(ctx context.Context, r slog.Record) error {
	scoped := cloneScopedSlogAttrs(h.scopedAttrs)
	if attrs := recordReservedDatadogAttrs(h.groups, r, true); len(attrs) > 0 {
		scoped = append(scoped, scopedSlogAttrs{groups: append([]string(nil), h.groups...), attrs: attrs})
	}
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	out.AddAttrs(materializeScopedSlogAttrs(scoped)...)
	return h.next.Handle(ctx, out)
}

func (h *stripReservedDatadogAttrsHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	filtered := filterReservedDatadogLogAttrs(h.groups, attrs, false)
	nextScoped := cloneScopedSlogAttrs(h.scopedAttrs)
	if len(filtered) > 0 {
		nextScoped = append(nextScoped, scopedSlogAttrs{groups: append([]string(nil), h.groups...), attrs: append([]slog.Attr(nil), filtered...)})
	}
	return &stripReservedDatadogAttrsHandler{next: h.next, scopedAttrs: nextScoped, groups: append([]string(nil), h.groups...)}
}

func (h *stripReservedDatadogAttrsHandler) WithGroup(name string) slog.Handler {
	return &stripReservedDatadogAttrsHandler{next: h.next, scopedAttrs: cloneScopedSlogAttrs(h.scopedAttrs), groups: appendRawGroup(h.groups, name)}
}
