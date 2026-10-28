package runtime_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kenmoini/mcp-machine/internal/runtime"
)

func TestDetectSelf(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "containerenv")
	if err := os.WriteFile(good, []byte("engine=\"podman-5.7.1\"\nid=\"abc123\"\nname=\"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if id, ok := runtime.DetectSelf(good); !ok || id != "abc123" {
		t.Fatalf("DetectSelf = (%q, %v), want (abc123, true)", id, ok)
	}

	noID := filepath.Join(dir, "noid")
	if err := os.WriteFile(noID, []byte("engine=\"podman-5.7.1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if id, ok := runtime.DetectSelf(noID); ok || id != "" {
		t.Fatalf("no id line: DetectSelf = (%q, %v), want (\"\", false)", id, ok)
	}

	emptyID := filepath.Join(dir, "emptyid")
	if err := os.WriteFile(emptyID, []byte("id=\"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if id, ok := runtime.DetectSelf(emptyID); ok || id != "" {
		t.Fatalf("empty id: DetectSelf = (%q, %v), want (\"\", false)", id, ok)
	}

	if id, ok := runtime.DetectSelf(filepath.Join(dir, "missing")); ok || id != "" {
		t.Fatalf("missing file: DetectSelf = (%q, %v), want (\"\", false)", id, ok)
	}
}
