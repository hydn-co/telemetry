package telemetry

import (
	"context"
	"log/slog"
)

// forbiddenDatadogHostLogKeys are slog attribute keys that must never appear on
// exported logs: Datadog maps them to infrastructure hosts and bills per host.
// Keys are leaf names (slog passes the local key to ReplaceAttr inside groups).
var forbiddenDatadogHostLogKeys = map[string]struct{}{
	"host":              {},
	"hostname":          {},
	"host.name":         {},
	"datadog.host.name": {},
}

func isForbiddenDatadogHostAttrKey(key string) bool {
	_, ok := forbiddenDatadogHostLogKeys[key]
	return ok
}

// datadogHostStripReplaceAttr is for [slog.HandlerOptions.ReplaceAttr]: drop host-like
// keys so JSONHandler preformatted With() attrs and per-record attrs cannot set host.
func datadogHostStripReplaceAttr(_ []string, a slog.Attr) slog.Attr {
	if isForbiddenDatadogHostAttrKey(a.Key) {
		return slog.Attr{}
	}
	return a
}

// filterForbiddenHostAttrs returns a copy of attrs without forbidden host keys,
// recursively for [slog.KindGroup].
func filterForbiddenHostAttrs(attrs []slog.Attr) []slog.Attr {
	var out []slog.Attr
	for _, a := range attrs {
		a.Value = a.Value.Resolve()
		if isForbiddenDatadogHostAttrKey(a.Key) {
			continue
		}
		if a.Value.Kind() == slog.KindGroup {
			inner := filterForbiddenHostAttrs(a.Value.Group())
			if len(inner) == 0 {
				continue
			}
			out = append(out, slog.Attr{Key: a.Key, Value: slog.GroupValue(inner...)})
			continue
		}
		out = append(out, slog.Attr{Key: a.Key, Value: a.Value})
	}
	return out
}

// recordStripForbiddenHostAttrs rebuilds r without forbidden host attributes.
func recordStripForbiddenHostAttrs(r slog.Record) slog.Record {
	var attrs []slog.Attr
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a)
		return true
	})
	filtered := filterForbiddenHostAttrs(attrs)
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	out.AddAttrs(filtered...)
	return out
}

// stripDatadogHostHandler removes host-like attributes before delegating to the next
// handler (OTLP bridge). It filters With() attrs so they are never stored on the child.
type stripDatadogHostHandler struct {
	next slog.Handler
}

func (h *stripDatadogHostHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *stripDatadogHostHandler) Handle(ctx context.Context, r slog.Record) error {
	return h.next.Handle(ctx, recordStripForbiddenHostAttrs(r))
}

func (h *stripDatadogHostHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &stripDatadogHostHandler{next: h.next.WithAttrs(filterForbiddenHostAttrs(attrs))}
}

func (h *stripDatadogHostHandler) WithGroup(name string) slog.Handler {
	return &stripDatadogHostHandler{next: h.next.WithGroup(name)}
}
