package podman

import (
	"context"
	"log/slog"
	"sync"
	"testing"
)

// recordingHandler captures slog records.
type recordingHandler struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r.Clone())
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

// lines returns the "line" attribute of each record, asserting level, message and id.
func (h *recordingHandler) lines(t *testing.T, wantID string) []string {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, r := range h.recs {
		if r.Level != slog.LevelDebug {
			t.Errorf("level = %v, want Debug", r.Level)
		}
		if r.Message != "container stderr" {
			t.Errorf("message = %q", r.Message)
		}
		var line, id string
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "line":
				line = a.Value.String()
			case "id":
				id = a.Value.String()
			}
			return true
		})
		if id != wantID {
			t.Errorf("id = %q, want %q", id, wantID)
		}
		out = append(out, line)
	}
	return out
}

func TestStderrLogger(t *testing.T) {
	h := &recordingHandler{}
	w := stderrLogger(slog.New(h), "abc123")

	write := func(s string) {
		t.Helper()
		n, err := w.Write([]byte(s))
		if err != nil || n != len(s) {
			t.Fatalf("Write(%q) = %d, %v; want %d, nil", s, n, err, len(s))
		}
	}

	write("one\ntwo\r\npar")
	if got := h.lines(t, "abc123"); len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("after first write lines = %q, want [one two]", got)
	}

	write("tial") // still no newline: nothing new logged
	if got := h.lines(t, "abc123"); len(got) != 2 {
		t.Fatalf("partial line must be buffered, lines = %q", got)
	}

	write(" line\n\nlast") // completes the partial, then an empty line; "last" stays buffered
	got := h.lines(t, "abc123")
	want := []string{"one", "two", "partial line", ""}
	if len(got) != len(want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("lines = %q, want %q", got, want)
		}
	}
}
