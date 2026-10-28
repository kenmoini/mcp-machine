package config_test

import (
	"testing"

	"github.com/kenmoini/mcp-machine/internal/config"
)

func TestExampleConfigLoadsAndValidates(t *testing.T) {
	for _, v := range []string{"GHCR_USER", "GHCR_TOKEN", "GITHUB_TOKEN"} {
		t.Setenv(v, "x")
	}
	set, err := config.Layout{
		BaseFile:  "../../config.example.yaml",
		SystemDir: "../../config.example.d",
	}.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := config.Validate(set.Config); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(set.Config.Servers) != 4 {
		t.Errorf("servers = %d, want 4", len(set.Config.Servers))
	}
	o, ok := set.Origins["filesystem"]
	if !ok || len(o.Files) == 0 {
		t.Fatalf("no origin recorded for filesystem: %+v", set.Origins)
	}
	if got := o.Files[0].Layer; got != config.LayerSystem {
		t.Errorf("filesystem layer = %v, want %v", got, config.LayerSystem)
	}
}
