package sources_test

import (
	"testing"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/sources"
)

func TestRegistryAuthForLongestPrefix(t *testing.T) {
	regs := []config.Registry{
		{Registry: "ghcr.io", Username: "org", Password: "p1"},
		{Registry: "ghcr.io/kenmoini", Username: "ken", Password: "p2"},
		{Registry: "docker.io", Username: "hub", Password: "p3"},
		{Registry: "localhost:5000", Username: "loc", Password: "p4"},
	}
	cases := []struct {
		ref  string
		user string // "" means nil
	}{
		{"ghcr.io/kenmoini/x:1", "ken"},
		{"ghcr.io/other/x:1", "org"},
		{"node:22", "hub"},
		{"library/node", "hub"},
		{"docker.io/library/node", "hub"},
		{"localhost:5000/img", "loc"},
		{"quay.io/foo/bar", ""},
	}
	for _, c := range cases {
		got := sources.RegistryAuthFor(regs, c.ref)
		if c.user == "" {
			if got != nil {
				t.Errorf("%s: got %+v, want nil", c.ref, got)
			}
			continue
		}
		if got == nil || got.Username != c.user {
			t.Errorf("%s: got %+v, want user %s", c.ref, got, c.user)
		}
	}
	if got := sources.RegistryAuthFor(regs[:2], "docker.io/library/node"); got != nil {
		t.Errorf("docker.io without entry: got %+v, want nil", got)
	}
	if got := sources.RegistryAuthFor(regs[:2], "node:22"); got != nil {
		t.Errorf("bare ref without docker.io entry: got %+v, want nil", got)
	}
	if got := sources.RegistryAuthFor(regs, "ghcr.io/kenmoini"); got == nil || got.Username != "ken" {
		t.Errorf("exact registry match: got %+v", got)
	}
	// "ghcr.io/kenmoinix/y" must not match the "ghcr.io/kenmoini" entry.
	if got := sources.RegistryAuthFor(regs, "ghcr.io/kenmoinix/y"); got == nil || got.Username != "org" {
		t.Errorf("segment boundary: got %+v, want org", got)
	}
}
