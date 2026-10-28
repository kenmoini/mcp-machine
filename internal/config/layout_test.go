package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kenmoini/mcp-machine/internal/config"
)

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("servers: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveLayoutRootMode(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "config.yaml"))
	user := filepath.Join(t.TempDir(), "user.d")

	l, err := config.ResolveLayout(config.LayoutOptions{Path: root, UserDir: user})
	if err != nil {
		t.Fatalf("ResolveLayout: %v", err)
	}
	if want := filepath.Join(root, "config.yaml"); l.BaseFile != want {
		t.Errorf("BaseFile = %q, want %q", l.BaseFile, want)
	}
	if want := filepath.Join(root, "config.d"); l.SystemDir != want {
		t.Errorf("SystemDir = %q, want %q", l.SystemDir, want)
	}
	if l.UserDir != user {
		t.Errorf("UserDir = %q, want %q", l.UserDir, user)
	}
	if l.Fallback != "" {
		t.Errorf("Fallback = %q, want empty", l.Fallback)
	}
}

func TestResolveLayoutRootModeSystemDirOverride(t *testing.T) {
	root := t.TempDir()
	sys := filepath.Join(t.TempDir(), "sys.d")
	l, err := config.ResolveLayout(config.LayoutOptions{Path: root, SystemDir: sys})
	if err != nil {
		t.Fatalf("ResolveLayout: %v", err)
	}
	if l.SystemDir != sys {
		t.Errorf("SystemDir = %q, want %q", l.SystemDir, sys)
	}
}

func TestResolveLayoutMissingRootIsRootMode(t *testing.T) {
	l, err := config.ResolveLayout(config.LayoutOptions{Path: "/nonexistent/mcp-machine"})
	if err != nil {
		t.Fatalf("ResolveLayout: %v", err)
	}
	if want := "/nonexistent/mcp-machine/config.yaml"; l.BaseFile != want {
		t.Errorf("BaseFile = %q, want %q", l.BaseFile, want)
	}
	if want := "/nonexistent/mcp-machine/config.d"; l.SystemDir != want {
		t.Errorf("SystemDir = %q, want %q", l.SystemDir, want)
	}
}

func TestResolveLayoutFileMode(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, file)
	sys := filepath.Join(t.TempDir(), "sys.d")

	tests := []struct {
		name, system, wantSystem string
	}{
		{"no system dir", "", ""},
		{"system dir given", sys, sys},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l, err := config.ResolveLayout(config.LayoutOptions{Path: file, SystemDir: tc.system})
			if err != nil {
				t.Fatalf("ResolveLayout: %v", err)
			}
			if l.BaseFile != file {
				t.Errorf("BaseFile = %q, want %q", l.BaseFile, file)
			}
			if l.SystemDir != tc.wantSystem {
				t.Errorf("SystemDir = %q, want %q", l.SystemDir, tc.wantSystem)
			}
			if l.Fallback != "" {
				t.Errorf("Fallback = %q, want empty", l.Fallback)
			}
		})
	}
}

func TestResolveLayoutMissingYamlIsFileMode(t *testing.T) {
	for _, name := range []string{"missing.yml", "missing.yaml"} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), name)
			l, err := config.ResolveLayout(config.LayoutOptions{Path: p})
			if err != nil {
				t.Fatalf("ResolveLayout: %v", err)
			}
			if l.BaseFile != p {
				t.Errorf("BaseFile = %q, want %q", l.BaseFile, p)
			}
			if l.SystemDir != "" {
				t.Errorf("SystemDir = %q, want empty", l.SystemDir)
			}
		})
	}
}

func TestResolveLayoutLegacyFallback(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, legacy)
	if err := os.MkdirAll(filepath.Join(root, "config.d"), 0o755); err != nil {
		t.Fatal(err)
	}

	l, err := config.ResolveLayout(config.LayoutOptions{Path: root, LegacyFile: legacy})
	if err != nil {
		t.Fatalf("ResolveLayout: %v", err)
	}
	if l.BaseFile != legacy || l.Fallback != legacy {
		t.Errorf("BaseFile = %q, Fallback = %q, want both %q", l.BaseFile, l.Fallback, legacy)
	}
}

func TestResolveLayoutNoFallbackWhenBaseExists(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "config.yaml"))
	legacy := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, legacy)

	l, err := config.ResolveLayout(config.LayoutOptions{Path: root, LegacyFile: legacy})
	if err != nil {
		t.Fatalf("ResolveLayout: %v", err)
	}
	if l.Fallback != "" || l.BaseFile != filepath.Join(root, "config.yaml") {
		t.Errorf("BaseFile = %q, Fallback = %q, want admin base and no fallback", l.BaseFile, l.Fallback)
	}
}

func TestResolveLayoutNoFallbackWhenSystemDropInExists(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "config.d", "a.yaml"))
	legacy := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, legacy)

	l, err := config.ResolveLayout(config.LayoutOptions{Path: root, LegacyFile: legacy})
	if err != nil {
		t.Fatalf("ResolveLayout: %v", err)
	}
	if l.Fallback != "" {
		t.Errorf("Fallback = %q, want empty", l.Fallback)
	}
	if want := filepath.Join(root, "config.yaml"); l.BaseFile != want {
		t.Errorf("BaseFile = %q, want %q", l.BaseFile, want)
	}
}

func TestResolveLayoutNoFallbackWhenLegacyMissingOrDisabled(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(t.TempDir(), "config.yaml")
	for _, legacy := range []string{"", missing} {
		l, err := config.ResolveLayout(config.LayoutOptions{Path: root, LegacyFile: legacy})
		if err != nil {
			t.Fatalf("ResolveLayout(%q): %v", legacy, err)
		}
		if l.Fallback != "" {
			t.Errorf("LegacyFile %q: Fallback = %q, want empty", legacy, l.Fallback)
		}
	}
}

func TestResolveLayoutNoFallbackInFileMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "missing.yaml")
	legacy := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, legacy)
	l, err := config.ResolveLayout(config.LayoutOptions{Path: p, LegacyFile: legacy})
	if err != nil {
		t.Fatalf("ResolveLayout: %v", err)
	}
	if l.Fallback != "" || l.BaseFile != p {
		t.Errorf("BaseFile = %q, Fallback = %q, want %q and no fallback", l.BaseFile, l.Fallback, p)
	}
}

func TestResolveLayoutSearchedOrder(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(t.TempDir(), "legacy.yaml")
	user := filepath.Join(t.TempDir(), "user.d")

	l, err := config.ResolveLayout(config.LayoutOptions{Path: root, UserDir: user, LegacyFile: legacy})
	if err != nil {
		t.Fatalf("ResolveLayout: %v", err)
	}
	want := []string{filepath.Join(root, "config.yaml"), filepath.Join(root, "config.d"), legacy, user}
	if !reflect.DeepEqual(l.Searched, want) {
		t.Errorf("Searched = %v, want %v", l.Searched, want)
	}
}

func TestResolveLayoutSearchedFileMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	l, err := config.ResolveLayout(config.LayoutOptions{Path: p})
	if err != nil {
		t.Fatalf("ResolveLayout: %v", err)
	}
	if want := []string{p}; !reflect.DeepEqual(l.Searched, want) {
		t.Errorf("Searched = %v, want %v", l.Searched, want)
	}
}

func TestResolveLayoutStatError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	p := filepath.Join(locked, "cfg")
	_, err := config.ResolveLayout(config.LayoutOptions{Path: p})
	if err == nil || !strings.HasPrefix(err.Error(), "config: stat "+p+": ") {
		t.Fatalf("err = %v, want config: stat %s: ...", err, p)
	}
}
