package podman

import (
	"reflect"
	"slices"
	"testing"

	"github.com/kenmoini/mcp-machine/internal/runtime"
)

func TestToSpecGen(t *testing.T) {
	s := runtime.ContainerSpec{
		Name: "mcpm-a", Image: "img", Command: []string{"npx", "x"}, Entrypoint: []string{"/bin/sh"},
		Env: map[string]string{"A": "1"}, Labels: map[string]string{"l": "v"},
		Stdin: true, Network: "mcp-machine", WorkDir: "/w",
		Ports: []runtime.PortMapping{{HostIP: "127.0.0.1", HostPort: 0, ContainerPort: 3000, Protocol: "tcp"}},
		Mounts: []runtime.Mount{
			{Type: "bind", Source: "/h", Target: "/c", ReadOnly: true},
			{Type: "bind", Source: "/h2", Target: "/c2"},
			{Type: "volume", Source: "vol", Target: "/v", ReadOnly: true},
		},
		RestartPolicy: "no",
	}
	g := toSpecGen(s)
	if g.Name != "mcpm-a" || g.Image != "img" || g.WorkDir != "/w" {
		t.Fatalf("name/image/workdir = %q %q %q", g.Name, g.Image, g.WorkDir)
	}
	if !reflect.DeepEqual(g.Command, []string{"npx", "x"}) || !reflect.DeepEqual(g.Entrypoint, []string{"/bin/sh"}) {
		t.Fatalf("command/entrypoint = %v %v", g.Command, g.Entrypoint)
	}
	if !reflect.DeepEqual(g.Env, map[string]string{"A": "1"}) || !reflect.DeepEqual(g.Labels, map[string]string{"l": "v"}) {
		t.Fatalf("env/labels = %v %v", g.Env, g.Labels)
	}
	if g.Stdin == nil || !*g.Stdin {
		t.Fatal("Stdin should be true")
	}
	if g.Terminal == nil || *g.Terminal {
		t.Fatal("Terminal should be false")
	}
	if _, ok := g.Networks["mcp-machine"]; !ok || len(g.Networks) != 1 {
		t.Fatalf("networks = %v", g.Networks)
	}
	if len(g.PortMappings) != 1 || g.PortMappings[0].ContainerPort != 3000 ||
		g.PortMappings[0].HostIP != "127.0.0.1" || g.PortMappings[0].HostPort != 0 || g.PortMappings[0].Protocol != "tcp" {
		t.Fatalf("ports = %+v", g.PortMappings)
	}
	if len(g.Mounts) != 2 {
		t.Fatalf("mounts = %+v", g.Mounts)
	}
	if m := g.Mounts[0]; m.Type != "bind" || m.Source != "/h" || m.Destination != "/c" || !slices.Contains(m.Options, "ro") || !slices.Contains(m.Options, "rbind") {
		t.Fatalf("mount0 = %+v", m)
	}
	if m := g.Mounts[1]; !slices.Contains(m.Options, "rw") || slices.Contains(m.Options, "ro") {
		t.Fatalf("mount1 = %+v", m)
	}
	if len(g.Volumes) != 1 || g.Volumes[0].Name != "vol" || g.Volumes[0].Dest != "/v" || !slices.Contains(g.Volumes[0].Options, "ro") {
		t.Fatalf("volumes = %+v", g.Volumes)
	}
	if g.RestartPolicy != "no" {
		t.Fatalf("restart = %q", g.RestartPolicy)
	}
}

func TestToSpecGenNoNetwork(t *testing.T) {
	g := toSpecGen(runtime.ContainerSpec{Name: "a", Image: "img"})
	if len(g.Networks) != 0 {
		t.Fatalf("networks = %v", g.Networks)
	}
	if g.Stdin == nil || *g.Stdin {
		t.Fatal("Stdin should be non-nil false")
	}
}

func TestLabelFilter(t *testing.T) {
	got := labelFilter(map[string]string{"b": "2", "a": "1"})
	want := map[string][]string{"label": {"a=1", "b=2"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got := labelFilter(nil); len(got) != 0 {
		t.Fatalf("nil labels: got %v", got)
	}
}

func TestToSpecGenRelabel(t *testing.T) {
	g := toSpecGen(runtime.ContainerSpec{Image: "img", Mounts: []runtime.Mount{
		{Type: "bind", Source: "/h", Target: "/c", ReadOnly: true, Relabel: "z"},
		{Type: "bind", Source: "/p", Target: "/p", Relabel: "Z"},
		{Type: "volume", Source: "vol", Target: "/v", Relabel: "z"},
		{Type: "bind", Source: "/n", Target: "/n"},
	}})
	if want := []string{"rbind", "ro", "z"}; !reflect.DeepEqual(g.Mounts[0].Options, want) {
		t.Errorf("mount0 options = %v, want %v", g.Mounts[0].Options, want)
	}
	if want := []string{"rbind", "rw", "Z"}; !reflect.DeepEqual(g.Mounts[1].Options, want) {
		t.Errorf("mount1 options = %v, want %v", g.Mounts[1].Options, want)
	}
	if want := []string{"rbind", "rw"}; !reflect.DeepEqual(g.Mounts[2].Options, want) {
		t.Errorf("mount2 options = %v, want %v", g.Mounts[2].Options, want)
	}
	if want := []string{"rw", "z"}; !reflect.DeepEqual(g.Volumes[0].Options, want) {
		t.Errorf("volume options = %v, want %v", g.Volumes[0].Options, want)
	}
}
