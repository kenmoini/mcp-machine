package cli

import (
	"bytes"
	"testing"
)

func TestNopWriteCloser(t *testing.T) {
	var buf bytes.Buffer
	w := nopWriteCloser{&buf}
	if _, err := w.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
	// Close is a no-op: the underlying writer stays usable.
	if _, err := w.Write([]byte("!")); err != nil {
		t.Fatalf("Write after Close: %v", err)
	}
	if got := buf.String(); got != "hi!" {
		t.Fatalf("buffer = %q, want %q", got, "hi!")
	}
}
