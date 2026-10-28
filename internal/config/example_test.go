package config_test

import (
	"os"
	"testing"

	"github.com/kenmoini/mcp-machine/internal/config"
)

func TestExampleConfigLoadsAndValidates(t *testing.T) {
	data, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Parse(data, func(string) (string, bool) { return "x", true })
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := config.Validate(c); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(c.Servers) != 4 {
		t.Errorf("servers = %d, want 4", len(c.Servers))
	}
}
