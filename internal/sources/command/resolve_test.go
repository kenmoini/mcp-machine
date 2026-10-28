package command

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/runtime"
	"github.com/kenmoini/mcp-machine/internal/sources"
)

func testConfig() *config.Config {
	return &config.Config{
		Podman: config.PodmanConfig{Network: "mcpm-net"},
		Runtimes: map[string]string{
			"npx":    "node:22",
			"uvx":    "ghcr.io/astral-sh/uv:python3.12",
			"python": "python:3.12",
		},
		Registries: []config.Registry{{Registry: "ghcr.io", Username: "u", Password: "p"}},
	}
}

func TestResolveNpx(t *testing.T) {
	cfg := testConfig()
	spec := config.ServerSpec{
		Name:    "a",
		Command: &config.CommandSpec{Command: "npx", Args: []string{"-y", "foo"}, Env: map[string]string{"A": "cmd", "B": "cmd"}},
		Env:     map[string]string{"B": "spec", "C": "spec"},
	}
	res, err := Resolve(cfg, spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != sources.KindContainer || res.Transport != "stdio" {
		t.Fatalf("kind/transport = %v/%q", res.Kind, res.Transport)
	}
	if res.Image.Ref != "node:22" || res.Image.Auth != nil {
		t.Fatalf("image = %+v", res.Image)
	}
	c := res.Container
	if c == nil {
		t.Fatal("no container")
	}
	if c.Name != "mcpm-a" || c.Image != "node:22" || !c.Stdin || c.Network != "mcpm-net" {
		t.Fatalf("container = %+v", c)
	}
	if want := []string{"npx", "-y", "foo"}; !reflect.DeepEqual(c.Command, want) {
		t.Fatalf("command = %v, want %v", c.Command, want)
	}
	wantMount := runtime.Mount{Type: "volume", Source: "mcpm-cache-npm", Target: "/root/.npm"}
	if !reflect.DeepEqual(c.Mounts, []runtime.Mount{wantMount}) {
		t.Fatalf("mounts = %+v", c.Mounts)
	}
	wantEnv := map[string]string{"A": "cmd", "B": "spec", "C": "spec"}
	if !reflect.DeepEqual(c.Env, wantEnv) {
		t.Fatalf("env = %v, want %v", c.Env, wantEnv)
	}
	if len(c.Labels) != 0 {
		t.Fatalf("labels = %v, want empty", c.Labels)
	}
}

func TestResolveUvx(t *testing.T) {
	cfg := testConfig()
	spec := config.ServerSpec{Name: "u", Command: &config.CommandSpec{Command: "uvx", Args: []string{"tool"}}}
	res, err := Resolve(cfg, spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Image.Ref != "ghcr.io/astral-sh/uv:python3.12" || res.Image.Auth == nil || res.Image.Auth.Username != "u" {
		t.Fatalf("image = %+v", res.Image)
	}
	want := runtime.Mount{Type: "volume", Source: "mcpm-cache-uv", Target: "/root/.cache/uv"}
	if !reflect.DeepEqual(res.Container.Mounts, []runtime.Mount{want}) {
		t.Fatalf("mounts = %+v", res.Container.Mounts)
	}
	if want := []string{"uvx", "tool"}; !reflect.DeepEqual(res.Container.Command, want) {
		t.Fatalf("command = %v", res.Container.Command)
	}
	// python also uses the uv cache.
	res, err = Resolve(cfg, config.ServerSpec{Name: "p", Command: &config.CommandSpec{Command: "python", Args: []string{"-m", "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Container.Mounts, []runtime.Mount{want}) {
		t.Fatalf("python mounts = %+v", res.Container.Mounts)
	}
}

func TestResolveRunLine(t *testing.T) {
	cfg := testConfig()
	spec := config.ServerSpec{
		Name: "r",
		Command: &config.CommandSpec{
			Run: "docker run -i --rm --name ignored --network other -w /w -e FOO=bar -v /h:/c:ro -v /a:/b -p 8080:80 -p 127.0.0.1:9000:9000/udp img --x",
		},
		Env: map[string]string{"S": "1"},
	}
	res, err := Resolve(cfg, spec)
	if err != nil {
		t.Fatal(err)
	}
	c := res.Container
	if res.Image.Ref != "img" || c.Image != "img" {
		t.Fatalf("image = %+v / %q", res.Image, c.Image)
	}
	if want := []string{"--x"}; !reflect.DeepEqual(c.Command, want) {
		t.Fatalf("command = %v", c.Command)
	}
	if want := map[string]string{"FOO": "bar", "S": "1"}; !reflect.DeepEqual(c.Env, want) {
		t.Fatalf("env = %v", c.Env)
	}
	wantMounts := []runtime.Mount{
		{Type: "bind", Source: "/h", Target: "/c", ReadOnly: true},
		{Type: "bind", Source: "/a", Target: "/b"},
	}
	if !reflect.DeepEqual(c.Mounts, wantMounts) {
		t.Fatalf("mounts = %+v", c.Mounts)
	}
	wantPorts := []runtime.PortMapping{
		{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
		{HostIP: "127.0.0.1", HostPort: 9000, ContainerPort: 9000, Protocol: "udp"},
	}
	if !reflect.DeepEqual(c.Ports, wantPorts) {
		t.Fatalf("ports = %+v", c.Ports)
	}
	if c.WorkDir != "/w" || c.Name != "mcpm-r" || c.Network != "mcpm-net" || !c.Stdin {
		t.Fatalf("container = %+v", c)
	}
}

func TestResolveRunLinePassEnv(t *testing.T) {
	old := lookupEnv
	t.Cleanup(func() { lookupEnv = old })
	lookupEnv = func(k string) (string, bool) {
		if k == "TOKEN" {
			return "secret", true
		}
		return "", false
	}
	cfg := testConfig()
	spec := config.ServerSpec{Name: "r", Command: &config.CommandSpec{Run: "docker run -i -e TOKEN img"}}
	res, err := Resolve(cfg, spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Container.Env["TOKEN"] != "secret" {
		t.Fatalf("env = %v", res.Container.Env)
	}
	spec.Command.Run = "docker run -i -e MISSING img"
	_, err = Resolve(cfg, spec)
	if err == nil || !strings.Contains(err.Error(), "environment variable MISSING is not set") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveRunLineError(t *testing.T) {
	_, err := Resolve(testConfig(), config.ServerSpec{Name: "r", Command: &config.CommandSpec{Run: "docker run -it img"}})
	if err == nil {
		t.Fatal("want error for tty")
	}
	_, err = Resolve(testConfig(), config.ServerSpec{Name: "r", Command: &config.CommandSpec{Run: "docker run -p bad img"}})
	if err == nil {
		t.Fatal("want error for bad port")
	}
	_, err = Resolve(testConfig(), config.ServerSpec{Name: "r", Command: &config.CommandSpec{Run: "docker run -v nocolon img"}})
	if err == nil {
		t.Fatal("want error for bad volume")
	}
}

func TestResolveOverrides(t *testing.T) {
	cfg := testConfig()
	spec := config.ServerSpec{
		Name:    "o",
		Command: &config.CommandSpec{Command: "npx", Args: []string{"x"}, Env: map[string]string{"K": "cmd"}},
		Env:     map[string]string{"K": "spec"},
		Container: &config.ContainerOverrides{
			Image:   "custom:1",
			Volumes: []string{"/data:/data", "/ro:/ro:ro"},
			Env:     map[string]string{"K": "container"},
		},
	}
	res, err := Resolve(cfg, spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Image.Ref != "custom:1" || res.Container.Image != "custom:1" {
		t.Fatalf("image = %+v / %q", res.Image, res.Container.Image)
	}
	if res.Container.Env["K"] != "container" {
		t.Fatalf("env = %v", res.Container.Env)
	}
	want := []runtime.Mount{
		{Type: "volume", Source: "mcpm-cache-npm", Target: "/root/.npm"},
		{Type: "bind", Source: "/data", Target: "/data"},
		{Type: "bind", Source: "/ro", Target: "/ro", ReadOnly: true},
	}
	if !reflect.DeepEqual(res.Container.Mounts, want) {
		t.Fatalf("mounts = %+v", res.Container.Mounts)
	}
}

func TestResolveUnknownCommand(t *testing.T) {
	_, err := Resolve(testConfig(), config.ServerSpec{Name: "x", Command: &config.CommandSpec{Command: "ruby"}})
	if err == nil || !strings.Contains(err.Error(), `unknown command "ruby"; add it to runtimes`) {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveNoCommand(t *testing.T) {
	if _, err := Resolve(testConfig(), config.ServerSpec{Name: "x"}); err == nil {
		t.Fatal("want error for nil Command")
	}
	if _, err := Resolve(testConfig(), config.ServerSpec{Name: "x", Command: &config.CommandSpec{}}); err == nil {
		t.Fatal("want error for empty Command")
	}
}

// fakeEnv replaces lookupEnv for the test with vars.
func fakeEnv(t *testing.T, vars map[string]string) {
	t.Helper()
	old := lookupEnv
	t.Cleanup(func() { lookupEnv = old })
	lookupEnv = func(k string) (string, bool) {
		v, ok := vars[k]
		return v, ok
	}
}

func TestResolveRunLineExpandsVars(t *testing.T) {
	fakeEnv(t, map[string]string{"TOKEN": "s3cret", "ORG": "acme"})
	spec := config.ServerSpec{Name: "r", Command: &config.CommandSpec{
		Run: `docker run -i -e GITHUB_TOKEN=${TOKEN} -e LABEL="org-${ORG}" img --org ${ORG} --bare $ORG --quoted '${ORG}x'`,
	}}
	res, err := Resolve(testConfig(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"GITHUB_TOKEN": "s3cret", "LABEL": "org-acme"}; !reflect.DeepEqual(res.Container.Env, want) {
		t.Fatalf("env = %v, want %v", res.Container.Env, want)
	}
	// The run line is parsed as a shell line: $ORG is a reference too, and
	// reaches the resolver as ${ORG}.
	if want := []string{"--org", "acme", "--bare", "acme", "--quoted", "acmex"}; !reflect.DeepEqual(res.Container.Command, want) {
		t.Fatalf("command = %v, want %v", res.Container.Command, want)
	}
}

func TestResolveRunLineMissingVars(t *testing.T) {
	fakeEnv(t, map[string]string{})
	spec := config.ServerSpec{Name: "r", Command: &config.CommandSpec{
		Run: `docker run -i -e A=${ONE} img ${TWO} ${ONE}`,
	}}
	_, err := Resolve(testConfig(), spec)
	if err == nil {
		t.Fatal("want an error for unset variables")
	}
	for _, want := range []string{"environment variable ONE is not set", "environment variable TWO is not set"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to contain %q", err, want)
		}
	}
	if n := strings.Count(err.Error(), "ONE"); n != 1 {
		t.Errorf("ONE reported %d times, want once: %v", n, err)
	}
}

func TestResolveCommandArgsExpandVars(t *testing.T) {
	fakeEnv(t, map[string]string{"DB": "/data/db.sqlite"})
	spec := config.ServerSpec{Name: "a", Command: &config.CommandSpec{Command: "uvx", Args: []string{"mcp-sqlite", "--db-path", "${DB}"}}}
	res, err := Resolve(testConfig(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"uvx", "mcp-sqlite", "--db-path", "/data/db.sqlite"}; !reflect.DeepEqual(res.Container.Command, want) {
		t.Fatalf("command = %v, want %v", res.Container.Command, want)
	}

	spec.Command.Args = []string{"${NOPE}"}
	_, err = Resolve(testConfig(), spec)
	if err == nil || !strings.Contains(err.Error(), "environment variable NOPE is not set") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveRunLinePassEnvFromSpec(t *testing.T) {
	// -e NAME takes its value from command.env or the server env before the
	// process environment.
	fakeEnv(t, map[string]string{"A": "process", "B": "process"})
	spec := config.ServerSpec{
		Name: "r",
		Command: &config.CommandSpec{
			Run: "docker run -i -e A -e B -e C img",
			Env: map[string]string{"A": "command"},
		},
		Env: map[string]string{"C": "server"},
	}
	res, err := Resolve(testConfig(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"A": "command", "B": "process", "C": "server"}; !reflect.DeepEqual(res.Container.Env, want) {
		t.Fatalf("env = %v, want %v", res.Container.Env, want)
	}
}

func TestParseVolume(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want runtime.Mount
	}{
		{"/h:/c", runtime.Mount{Type: "bind", Source: "/h", Target: "/c"}},
		{"/h:/c:ro", runtime.Mount{Type: "bind", Source: "/h", Target: "/c", ReadOnly: true}},
		{"/h:/c:rw", runtime.Mount{Type: "bind", Source: "/h", Target: "/c"}},
		{"/h:/c:z", runtime.Mount{Type: "bind", Source: "/h", Target: "/c", Relabel: "z"}},
		{"/h:/c:Z", runtime.Mount{Type: "bind", Source: "/h", Target: "/c", Relabel: "Z"}},
		{"/h:/c:ro,z", runtime.Mount{Type: "bind", Source: "/h", Target: "/c", ReadOnly: true, Relabel: "z"}},
		{"/h:/c:Z,ro", runtime.Mount{Type: "bind", Source: "/h", Target: "/c", ReadOnly: true, Relabel: "Z"}},
		{"./rel:/c", runtime.Mount{Type: "bind", Source: "./rel", Target: "/c"}},
		{"data:/data", runtime.Mount{Type: "volume", Source: "data", Target: "/data"}},
		{"data:/data:ro", runtime.Mount{Type: "volume", Source: "data", Target: "/data", ReadOnly: true}},
	} {
		got, err := ParseVolume(tc.in)
		if err != nil {
			t.Errorf("ParseVolume(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseVolume(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"nocolon", ":/c", "/h:", "/h:/c:ro:z", "/h:/c:bogus", "/h:/c:ro,rw", "/h:/c:z,Z", "/h:/c:ro,"} {
		if _, err := ParseVolume(bad); err == nil {
			t.Errorf("ParseVolume(%q): want an error", bad)
		}
	}
}

func TestResolveRunLineMountsAndInit(t *testing.T) {
	spec := config.ServerSpec{Name: "m", Command: &config.CommandSpec{
		Run: "docker run -i --init -v /h:/c:ro,z -v cache:/cache --mount type=bind,src=/a,dst=/b,readonly img",
	}}
	res, err := Resolve(testConfig(), spec)
	if err != nil {
		t.Fatal(err)
	}
	want := []runtime.Mount{
		{Type: "bind", Source: "/h", Target: "/c", ReadOnly: true, Relabel: "z"},
		{Type: "volume", Source: "cache", Target: "/cache"},
		{Type: "bind", Source: "/a", Target: "/b", ReadOnly: true},
	}
	if !reflect.DeepEqual(res.Container.Mounts, want) {
		t.Fatalf("mounts = %+v, want %+v", res.Container.Mounts, want)
	}
}
