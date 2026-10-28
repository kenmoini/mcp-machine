package resolver

import (
	"context"
	"strings"
	"testing"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/sources"
)

func testCfg() *config.Config {
	return &config.Config{Runtimes: map[string]string{"npx": "node:22"}}
}

func TestResolverDispatches(t *testing.T) {
	tests := []struct {
		name          string
		spec          config.ServerSpec
		kind          sources.Kind
		transport     string
		wantBuildPlan bool
	}{
		{"remote", config.ServerSpec{Name: "r", Type: "remote", Remote: &config.RemoteSpec{URL: "https://example.com/mcp"}}, sources.KindRemote, "", false},
		{"command", config.ServerSpec{Name: "c", Type: "command", Command: &config.CommandSpec{Command: "npx", Args: []string{"x"}}}, sources.KindContainer, "stdio", false},
		{"build", config.ServerSpec{Name: "b", Type: "build", Build: &config.BuildSpec{Repo: "https://example.com/r.git"}}, sources.KindContainer, "stdio", true},
	}
	r := New()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := r.Resolve(context.Background(), testCfg(), tc.spec)
			if err != nil {
				t.Fatal(err)
			}
			if res.Kind != tc.kind || res.Transport != tc.transport {
				t.Fatalf("kind/transport = %v/%q", res.Kind, res.Transport)
			}
			if (res.Image.Build != nil) != tc.wantBuildPlan {
				t.Fatalf("image = %+v", res.Image)
			}
		})
	}
}

func TestResolverWrapsErrors(t *testing.T) {
	spec := config.ServerSpec{Name: "broken", Type: "remote"}
	_, err := New().Resolve(context.Background(), testCfg(), spec)
	if err == nil || !strings.HasPrefix(err.Error(), `server "broken": `) || !strings.Contains(err.Error(), "remote is required") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolverUnknownType(t *testing.T) {
	_, err := New().Resolve(context.Background(), testCfg(), config.ServerSpec{Name: "x", Type: "magic"})
	if err == nil || !strings.HasPrefix(err.Error(), `server "x": `) || !strings.Contains(err.Error(), `"magic"`) {
		t.Fatalf("err = %v", err)
	}
}
