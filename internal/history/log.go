package history

import (
	"context"
	"log/slog"
	"maps"
)

// LogHandler wraps an existing slog.Handler: every record is forwarded to it
// unchanged, and additionally, Warn/Error-level records are appended to the
// Store's event ring. No existing log.Warn/log.Error call site needs to
// change for its output to show up in the dashboard.
type LogHandler struct {
	next  slog.Handler
	store *Store
	attrs map[string]any
	group string
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
		maps.Copy(attrs, h.attrs)
		r.Attrs(func(a slog.Attr) bool {
			addAttr(attrs, h.group, a)
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
	merged := map[string]any{}
	maps.Copy(merged, h.attrs)
	for _, a := range attrs {
		addAttr(merged, h.group, a)
	}
	return &LogHandler{next: next, store: h.store, attrs: merged, group: h.group}
}

func (h *LogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := h.next.WithGroup(name)
	return &LogHandler{next: next, store: h.store, attrs: h.attrs, group: h.group + name + "."}
}

func addAttr(dst map[string]any, group string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		if a.Key != "" {
			group += a.Key + "."
		}
		for _, sub := range a.Value.Group() {
			addAttr(dst, group, sub)
		}
		return
	}
	value := a.Value.Any()
	if err, ok := value.(error); ok {
		value = err.Error()
	}
	dst[group+a.Key] = value
}
