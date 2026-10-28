package config_test

import (
	"testing"

	"github.com/kenmoini/mcp-machine/internal/config"
)

func boolPtr(b bool) *bool { return &b }

func hashSpec() config.ServerSpec {
	return config.ServerSpec{
		Name: "fs",
		Type: "command",
		Command: &config.CommandSpec{
			Command: "npx",
			Args:    []string{"-y", "@modelcontextprotocol/server-filesystem", "/data"},
			Env:     map[string]string{"A": "1", "B": "2", "C": "3"},
		},
		Serve: &config.ServeSpec{Mode: "stdio-bridge", Port: 8080, Path: "/mcp", Transport: "streamable-http"},
		Env:   map[string]string{"X": "1", "Y": "2", "Z": "3"},
	}
}

func TestHashIgnoresMapOrderAndEnabled(t *testing.T) {
	a := hashSpec()
	a.Enabled = boolPtr(true)

	// Same content, maps built by inserting keys in the opposite order.
	b := hashSpec()
	b.Env = map[string]string{}
	for _, k := range []string{"Z", "Y", "X"} {
		b.Env[k] = map[string]string{"X": "1", "Y": "2", "Z": "3"}[k]
	}
	b.Command.Env = map[string]string{}
	for _, k := range []string{"C", "B", "A"} {
		b.Command.Env[k] = map[string]string{"A": "1", "B": "2", "C": "3"}[k]
	}
	b.Enabled = boolPtr(false)

	if got, want := config.Hash(a, "img"), config.Hash(b, "img"); got != want {
		t.Fatalf("hash differs: %s vs %s", got, want)
	}

	// Unset Enabled must also match.
	c := hashSpec()
	if got, want := config.Hash(c, "img"), config.Hash(a, "img"); got != want {
		t.Fatalf("hash differs for nil Enabled: %s vs %s", got, want)
	}
}

func TestHashDoesNotMutateInput(t *testing.T) {
	s := hashSpec()
	s.Enabled = boolPtr(true)
	config.Hash(s, "img")
	if s.Enabled == nil || !*s.Enabled {
		t.Fatal("Hash modified the caller's spec")
	}
}

func TestHashChangesWhenArgsChange(t *testing.T) {
	a := hashSpec()
	b := hashSpec()
	b.Command.Args = append(b.Command.Args, "/other")
	if config.Hash(a, "img") == config.Hash(b, "img") {
		t.Fatal("hash should differ when args differ")
	}
}

func TestHashChangesWithRuntimeImage(t *testing.T) {
	s := hashSpec()
	if config.Hash(s, "node:20") == config.Hash(s, "node:22") {
		t.Fatal("hash should differ when runtime image differs")
	}
}

func TestHashIsStable(t *testing.T) {
	// Golden value produced by hashSpec() with runtime image "node:22".
	// If this fails, the canonical form changed and every running container
	// will be recreated on next reconcile; change it only deliberately.
	const golden = "abeb635e1ed1218789831b009b57a2b6ecabfc260810197c29e64232b19e05cf"
	if got := config.Hash(hashSpec(), "node:22"); got != golden {
		t.Fatalf("Hash = %q, want %q", got, golden)
	}
}

func TestHashStableAcrossLayers(t *testing.T) {
	headers := "      transport: sse\n      headers: {A: \"1\", B: \"2\"}\n"
	one := layoutDir(t, map[string]string{"config.yaml": remoteYAML("a", headers)})
	split := layoutDir(t, map[string]string{
		"config.yaml":     remoteYAML("a", ""),
		"config.d/a.yaml": "servers:\n  - name: a\n    remote:\n" + headers,
	})
	hash := func(l config.Layout) string {
		t.Helper()
		set, err := l.Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		s := set.Config.Servers[0]
		return config.Hash(s, s.RuntimeImage(set.Config))
	}
	if a, b := hash(one), hash(split); a != b {
		t.Errorf("hash differs across layers: %s vs %s", a, b)
	}
}
