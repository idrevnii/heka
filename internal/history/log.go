package history

import (
	"context"
	"log/slog"
)

// LogHandler wraps an existing slog.Handler: every record is forwarded to it
// unchanged, and additionally, Warn/Error-level records are appended to the
// Store's event ring. No existing log.Warn/log.Error call site needs to
// change for its output to show up in the dashboard.
type LogHandler struct {
	next   slog.Handler
	store  *Store
	attrs  []slog.Attr
	groups []string
}

// NewLogHandler wraps next, capturing Warn/Error records into s.
func NewLogHandler(next slog.Handler, s *Store) *LogHandler {
	return &LogHandler{next: next, store: s}
}

func (h *LogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *LogHandler) Handle(ctx context.Context, r slog.Record) error {
	err := h.next.Handle(ctx, r)
	if r.Level >= slog.LevelWarn {
		attrs := map[string]any{}
		flattenAttrs(attrs, h.groups, h.attrs)
		r.Attrs(func(a slog.Attr) bool {
			addAttr(attrs, h.groups, a)
			return true
		})
		h.store.AddEvent(Event{
			Time:  r.Time,
			Level: r.Level.String(),
			Msg:   r.Message,
			Attrs: attrs,
		})
	}
	return err
}

func (h *LogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := h.next.WithAttrs(attrs)
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)
	return &LogHandler{next: next, store: h.store, attrs: merged, groups: h.groups}
}

func (h *LogHandler) WithGroup(name string) slog.Handler {
	next := h.next.WithGroup(name)
	groups := make([]string, 0, len(h.groups)+1)
	groups = append(groups, h.groups...)
	groups = append(groups, name)
	return &LogHandler{next: next, store: h.store, attrs: h.attrs, groups: groups}
}

func flattenAttrs(dst map[string]any, groups []string, attrs []slog.Attr) {
	for _, a := range attrs {
		addAttr(dst, groups, a)
	}
}

func addAttr(dst map[string]any, groups []string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	key := a.Key
	for i := len(groups) - 1; i >= 0; i-- {
		key = groups[i] + "." + key
	}
	if a.Value.Kind() == slog.KindGroup {
		for _, sub := range a.Value.Group() {
			addAttr(dst, append(groups, a.Key), sub)
		}
		return
	}
	dst[key] = a.Value.Any()
}
