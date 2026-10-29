package cli

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kenmoini/mcp-machine/internal/config"
)

func TestValidateVaultFlag(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// No references: --vault is a no-op and never contacts Vault.
	plain := write("plain.yaml", "servers:\n  - {name: a, type: remote, remote: {url: https://x/mcp}}\n")
	var out, errb strings.Builder
	if code := runValidate([]string{"-c", plain, "--config-d", "", "--vault"}, &out, &errb); code != exitOK {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}

	// A reference with an unreachable Vault: reported, exit 1.
	refs := write("refs.yaml", "vault: {address: \"http://127.0.0.1:1\", token: t}\n"+
		"servers:\n  - {name: a, type: command, command: {command: npx}, env: {T: \"${vault:kv/data/a#k}\"}}\n")
	out.Reset()
	errb.Reset()
	if code := runValidate([]string{"-c", refs, "--config-d", "", "--vault"}, &out, &errb); code != exitRuntime {
		t.Fatalf("code = %d, want 1; stdout = %q", code, out.String())
	}
	if !strings.Contains(errb.String(), "vault: lookup token:") {
		t.Errorf("stderr = %q, want the lookup failure", errb.String())
	}
	// Without --vault the same file validates.
	out.Reset()
	errb.Reset()
	if code := runValidate([]string{"-c", refs, "--config-d", ""}, &out, &errb); code != exitOK {
		t.Fatalf("without --vault: code = %d, stderr = %s", code, errb.String())
	}
}

func TestVaultProblemsWithoutAddress(t *testing.T) {
	cfg := &config.Config{Servers: []config.ServerSpec{{Name: "a", Env: map[string]string{"T": "${vault:kv/data/a#k}"}}}}
	got := vaultProblems(context.Background(), cfg)
	if len(got) != 1 || !strings.Contains(got[0], "vault is not configured") {
		t.Errorf("problems = %v", got)
	}
	if got := vaultProblems(context.Background(), &config.Config{}); got != nil {
		t.Errorf("no refs: %v", got)
	}
}

func TestNewVaultClient(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	if vc, err := newVaultClient(context.Background(), config.VaultConfig{}, logger); vc != nil || err != nil {
		t.Errorf("unconfigured: %v %v", vc, err)
	}
	// Unreachable Vault: the client is kept, the failure only logged.
	vc, err := newVaultClient(context.Background(), config.VaultConfig{Address: "http://127.0.0.1:1", Token: "t"}, logger)
	if err != nil || vc == nil {
		t.Fatalf("unreachable: %v %v", vc, err)
	}
	vc.Close()
	// A credential file that cannot be read is fatal.
	_, err = newVaultClient(context.Background(), config.VaultConfig{Address: "http://127.0.0.1:1", TokenFile: filepath.Join(t.TempDir(), "nope")}, logger)
	if err == nil || !strings.Contains(err.Error(), "read token file") {
		t.Errorf("missing token file: %v", err)
	}
}

func TestCatalogSource(t *testing.T) {
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	if fn := catalogSource("", logger); fn != nil {
		t.Error(`"" should disable the catalog`)
	}
	missing := filepath.Join(t.TempDir(), "nope")
	fn := catalogSource(missing, logger)
	if fn == nil {
		t.Fatal("missing directory returned no source")
	}
	if got := fn(); got != nil {
		t.Errorf("missing directory yields entries: %v", got)
	}
	if !strings.Contains(logs.String(), "catalog directory not found") {
		t.Errorf("missing directory not logged: %s", logs.String())
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fetch.yaml"), []byte("# Fetch.\n# category: development\nservers:\n  - {name: fetch, type: command, command: {command: uvx}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := catalogSource(dir, logger)()
	if len(got) != 1 || got[0].Name != "fetch" || got[0].Description != "Fetch." {
		t.Errorf("entries = %+v", got)
	}
	if defaultCatalogDir != "/usr/share/mcp-machine/catalog" {
		t.Errorf("defaultCatalogDir = %q", defaultCatalogDir)
	}
}
