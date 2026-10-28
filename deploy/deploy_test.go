package deploy_test

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile("../" + rel)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func TestComposeMountsSocketAndData(t *testing.T) {
	var doc struct {
		Services map[string]struct {
			Volumes []string `yaml:"volumes"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(readRepoFile(t, "compose.yaml")), &doc); err != nil {
		t.Fatalf("parse compose.yaml: %v", err)
	}
	svc, ok := doc.Services["mcp-machine"]
	if !ok {
		t.Fatal(`compose.yaml has no service "mcp-machine"`)
	}
	var socket, data bool
	for _, v := range svc.Volumes {
		if v == "/run/podman/podman.sock:/run/podman/podman.sock" {
			socket = true
		}
		if strings.HasSuffix(v, ":/data") {
			data = true
		}
	}
	if !socket {
		t.Errorf("volumes %v missing the podman socket mount", svc.Volumes)
	}
	if !data {
		t.Errorf("volumes %v missing a :/data mount", svc.Volumes)
	}
}

func TestQuadletUnits(t *testing.T) {
	c := readRepoFile(t, "deploy/quadlet/mcp-machine.container")
	for _, want := range []string{
		"[Container]",
		"Image=ghcr.io/kenmoini/mcp-machine",
		"Network=mcp-machine.network",
		"Volume=/run/podman/podman.sock",
	} {
		if !strings.Contains(c, want) {
			t.Errorf(".container missing %q", want)
		}
	}
	n := readRepoFile(t, "deploy/quadlet/mcp-machine.network")
	if !strings.Contains(n, "NetworkName=mcp-machine") {
		t.Errorf(".network missing NetworkName=mcp-machine")
	}
}

func TestContainerfileStages(t *testing.T) {
	cf := readRepoFile(t, "Containerfile")
	var froms []string
	for _, l := range strings.Split(cf, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "FROM ") {
			froms = append(froms, l)
		}
	}
	if len(froms) < 2 {
		t.Fatalf("want at least 2 FROM lines, got %v", froms)
	}
	if !strings.Contains(froms[0], "ubi9/go-toolset:1.26") {
		t.Errorf("first FROM = %q", froms[0])
	}
	if !strings.Contains(froms[len(froms)-1], "ubi9/ubi-minimal:9.8") {
		t.Errorf("last FROM = %q", froms[len(froms)-1])
	}
	// The go-toolset base image sets ENV VERSION, so a bare ARG VERSION would
	// be overridden; the build args must be namespaced.
	for _, want := range []string{"ARG MCPM_VERSION", "ARG MCPM_COMMIT", "ARG MCPM_DATE"} {
		if !strings.Contains(cf, want) {
			t.Errorf("Containerfile missing %q", want)
		}
	}
	for _, l := range strings.Split(cf, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "ARG VERSION") {
			t.Errorf("Containerfile has bare %q", l)
		}
	}
	// The pinned `toolchain` in go.mod is newer than the base image's Go, so
	// the build stage must be allowed to fetch it.
	if !strings.Contains(cf, "GOTOOLCHAIN=auto") {
		t.Errorf("Containerfile build stage missing GOTOOLCHAIN=auto")
	}
}

func TestContainerfileCreatesAdminRootAndDefaultsToIt(t *testing.T) {
	cf := readRepoFile(t, "Containerfile")
	if !strings.Contains(cf, "mkdir -p /etc/mcp-machine/config.d") {
		t.Errorf("Containerfile does not create /etc/mcp-machine/config.d")
	}
	if !strings.Contains(cf, `CMD ["serve","-c","/etc/mcp-machine"]`) {
		t.Errorf("Containerfile default CMD does not read the admin root /etc/mcp-machine")
	}
	if strings.Contains(cf, "/data/config.yaml") {
		t.Errorf("Containerfile still refers to /data/config.yaml")
	}
}

func TestComposeMountsAdminRootReadOnly(t *testing.T) {
	var doc struct {
		Services map[string]struct {
			Volumes []string `yaml:"volumes"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(readRepoFile(t, "compose.yaml")), &doc); err != nil {
		t.Fatalf("parse compose.yaml: %v", err)
	}
	svc := doc.Services["mcp-machine"]
	for _, v := range svc.Volumes {
		if strings.HasSuffix(v, ":/etc/mcp-machine:ro") {
			return
		}
	}
	t.Errorf("volumes %v missing a read-only :/etc/mcp-machine:ro mount", svc.Volumes)
}

func TestQuadletMountsAdminRootReadOnly(t *testing.T) {
	c := readRepoFile(t, "deploy/quadlet/mcp-machine.container")
	if !strings.Contains(c, "Volume=/etc/mcp-machine:/etc/mcp-machine:ro") {
		t.Errorf(".container missing the read-only admin root mount")
	}
}

func TestMakeRunPassesConfigFlags(t *testing.T) {
	mk := readRepoFile(t, "Makefile")
	for _, want := range []string{
		"CONFIG ?= /etc/mcp-machine",
		"CONFIG_D ?= /data/config.d",
		"serve -c $(CONFIG) --config-d $(CONFIG_D)",
		"validate -c $(CONFIG) --config-d $(CONFIG_D)",
	} {
		if !strings.Contains(mk, want) {
			t.Errorf("Makefile missing %q", want)
		}
	}
}

func TestContainerignoreExcludesScratch(t *testing.T) {
	lines := map[string]bool{}
	for _, l := range strings.Split(readRepoFile(t, ".containerignore"), "\n") {
		lines[strings.TrimSpace(l)] = true
	}
	for _, want := range []string{".gocache/", ".superpowers/", "bin/"} {
		if !lines[want] {
			t.Errorf(".containerignore missing %q", want)
		}
	}
}

func TestMakeImageTagsLatest(t *testing.T) {
	// compose.yaml, the Quadlet unit and the README run :latest, so a local
	// `make image` must produce it.
	mk := readRepoFile(t, "Makefile")
	i := strings.Index(mk, "\nimage:")
	if i < 0 {
		t.Fatal("Makefile has no image target")
	}
	recipe := mk[i:]
	if j := strings.Index(recipe[1:], "\n\n"); j >= 0 {
		recipe = recipe[:j+1]
	}
	for _, want := range []string{"-t $(IMAGE):$(TAG)", "-t $(IMAGE):latest"} {
		if !strings.Contains(recipe, want) {
			t.Errorf("image recipe missing %q:\n%s", want, recipe)
		}
	}
}
