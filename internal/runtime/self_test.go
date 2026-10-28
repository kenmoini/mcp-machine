package runtime_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kenmoini/mcp-machine/internal/runtime"
	"github.com/kenmoini/mcp-machine/internal/runtime/fake"
)

func TestDetectSelf(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	cases := []struct {
		name, path  string
		wantID      string
		inContainer bool
	}{
		// Privileged containers get the full identity.
		{"id line", write("privileged", "engine=\"podman-5.7.1\"\nid=\"abc123\"\nname=\"x\"\n"), "abc123", true},
		// Non-privileged containers (podman run, compose, Quadlet) get an
		// empty file or only graphRootMounted=1: still in a container.
		{"empty file", write("empty", ""), "", true},
		{"graphRootMounted only", write("graphroot", "graphRootMounted=1\n"), "", true},
		{"engine only", write("engine", "engine=\"podman-5.7.1\"\n"), "", true},
		{"empty id", write("emptyid", "id=\"\"\n"), "", true},
		{"missing file", filepath.Join(dir, "missing"), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, in := runtime.DetectSelf(tc.path)
			if id != tc.wantID || in != tc.inContainer {
				t.Fatalf("DetectSelf = (%q, %v), want (%q, %v)", id, in, tc.wantID, tc.inContainer)
			}
		})
	}
}

func TestResolveSelfIDHint(t *testing.T) {
	eng := fake.New()
	id, err := runtime.ResolveSelfID(context.Background(), eng, "abc123")
	if err != nil || id != "abc123" {
		t.Fatalf("ResolveSelfID = (%q, %v), want (abc123, nil)", id, err)
	}
	if len(eng.Calls()) != 0 {
		t.Errorf("engine calls = %v, want none when the hint is set", eng.Calls())
	}
}

func TestResolveSelfIDFromHostname(t *testing.T) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Skipf("no host name: %v", err)
	}
	t.Setenv(runtime.SelfIDEnv, "from-env")
	eng := fake.New()
	eng.Seed(runtime.ContainerInfo{ID: "0123456789abcdef", Name: host, State: "running"})

	id, err := runtime.ResolveSelfID(context.Background(), eng, "")
	if err != nil || id != "0123456789abcdef" {
		t.Fatalf("ResolveSelfID = (%q, %v), want the inspected ID", id, err)
	}
}

func TestResolveSelfIDFromEnv(t *testing.T) {
	t.Setenv(runtime.SelfIDEnv, "mcp-machine")
	id, err := runtime.ResolveSelfID(context.Background(), fake.New(), "")
	if err != nil || id != "mcp-machine" {
		t.Fatalf("ResolveSelfID = (%q, %v), want (mcp-machine, nil)", id, err)
	}
}

func TestResolveSelfIDFails(t *testing.T) {
	t.Setenv(runtime.SelfIDEnv, "")
	id, err := runtime.ResolveSelfID(context.Background(), fake.New(), "")
	if err == nil || id != "" {
		t.Fatalf("ResolveSelfID = (%q, %v), want an error", id, err)
	}
}
