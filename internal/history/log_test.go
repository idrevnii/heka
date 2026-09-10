package history

import (
	"bytes"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
)

func TestLogHandlerCapturesWarnOnly(t *testing.T) {
	var buf bytes.Buffer
	next := slog.NewTextHandler(&buf, nil)
	s := New(10, 10, 1<<20)
	log := slog.New(NewLogHandler(next, s))

	log.Info("routine thing", "a", 1)
	log.Warn("something odd", "provider", "mock", "key", "abcd")
	log.Error("bad", "err", "boom")

	if !strings.Contains(buf.String(), "routine thing") {
		t.Fatal("Info should still reach the wrapped handler")
	}
	if !strings.Contains(buf.String(), "something odd") {
		t.Fatal("Warn should still reach the wrapped handler")
	}

	errs := s.Errors(10)
	if len(errs) != 2 {
		t.Fatalf("captured events = %d, want 2 (warn+error only)", len(errs))
	}
	// Newest first.
	if errs[0].Msg != "bad" || errs[1].Msg != "something odd" {
		t.Fatalf("unexpected events: %+v", errs)
	}
	if errs[1].Attrs["provider"] != "mock" {
		t.Fatalf("attrs not captured: %+v", errs[1].Attrs)
	}
}

func TestLogHandlerWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	next := slog.NewTextHandler(&buf, nil)
	s := New(10, 10, 1<<20)
	log := slog.New(NewLogHandler(next, s)).With("component", "sidecar")

	log.Warn("crashed")

	errs := s.Errors(10)
	if len(errs) != 1 {
		t.Fatalf("events = %d", len(errs))
	}
	if errs[0].Attrs["component"] != "sidecar" {
		t.Fatalf("With-attrs not carried into captured event: %+v", errs[0].Attrs)
	}
}

func TestLogHandlerPreservesAttributeGroups(t *testing.T) {
	var buf bytes.Buffer
	s := New(10, 10, 1024)
	log := slog.New(NewLogHandler(slog.NewTextHandler(&buf, nil), s)).With("root", "r").WithGroup("request").With("id", 7).WithGroup("nested")
	log.Warn("failed", slog.Group("", "inline", true), slog.Group("a", "x", 1), slog.Group("b", "x", 2), "error", errors.New("upstream down"))
	want := map[string]any{
		"root": "r", "request.id": int64(7), "request.nested.inline": true,
		"request.nested.a.x": int64(1), "request.nested.b.x": int64(2), "request.nested.error": "upstream down",
	}
	if got := s.Errors(1)[0].Attrs; !reflect.DeepEqual(got, want) {
		t.Fatalf("attrs=%v, want %v", got, want)
	}
}
