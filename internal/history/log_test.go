package history

import (
	"bytes"
	"log/slog"
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
