package catalog_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/sources/resolver"
)

// TestCatalogLoadsValidatesAndResolves loads every catalog drop-in as a
// system drop-in, validates the merged configuration, and resolves each
// server the way the manager would (which parses the run lines), so a
// catalog entry can never drift from the configuration schema.
func TestCatalogLoadsValidatesAndResolves(t *testing.T) {
	t.Setenv("VAULT_ADDR", "https://vault.test:8200") // the ${vault:...} references need an address
	t.Setenv("VAULT_TOKEN", "test")
	files, err := filepath.Glob("*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no catalog files: %v", err)
	}
	set, err := config.Layout{SystemDir: "."}.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := config.Validate(set.Config); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got, want := len(set.Config.Servers), len(files); got != want {
		t.Errorf("servers = %d, files = %d: one server per file", got, want)
	}
	r := resolver.New()
	for _, s := range set.Config.Servers {
		origin := set.Origins[s.Name]
		if len(origin.Files) != 1 || filepath.Base(origin.Files[0].Path) != s.Name+".yaml" {
			t.Errorf("server %q is defined in %v, want %s.yaml", s.Name, origin.Files, s.Name)
		}
		if _, err := r.Resolve(context.Background(), set.Config, s); err != nil {
			t.Errorf("server %q does not resolve: %v", s.Name, err)
		}
		// Every secret lives under one prefix, so one policy covers the catalog.
		refs, errs := config.CheckVaultRefs(s)
		if len(errs) != 0 {
			t.Errorf("server %q: %v", s.Name, errs)
		}
		for _, ref := range refs {
			if !strings.HasPrefix(ref.Path, "kv/data/mcp-machine/") {
				t.Errorf("server %q references %s outside kv/data/mcp-machine/", s.Name, ref)
			}
		}
	}
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // G304: catalog files from the glob above
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(strings.TrimLeft(string(b), "\n"), "#") {
			t.Errorf("%s has no leading comment saying what it is", f)
		}
	}
}
