package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kenmoini/mcp-machine/internal/config"
)

// layoutDir writes files (paths relative to a fresh temp root) and returns a
// Layout over <root>/config.yaml, <root>/config.d and <root>/user.d.
func layoutDir(t *testing.T, files map[string]string) config.Layout {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	l := config.Layout{
		BaseFile:  filepath.Join(root, "config.yaml"),
		SystemDir: filepath.Join(root, "config.d"),
		UserDir:   filepath.Join(root, "user.d"),
	}
	l.Searched = []string{l.BaseFile, l.SystemDir, l.UserDir}
	return l
}

func mustLoad(t *testing.T, l config.Layout) *config.Set {
	t.Helper()
	set, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return set
}

func serverNames(c *config.Config) []string {
	names := make([]string, 0, len(c.Servers))
	for _, s := range c.Servers {
		names = append(names, s.Name)
	}
	return names
}

func server(t *testing.T, c *config.Config, name string) config.ServerSpec {
	t.Helper()
	for _, s := range c.Servers {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("server %q not found in %v", name, serverNames(c))
	return config.ServerSpec{}
}

func remoteYAML(name, extra string) string {
	return "servers:\n  - name: " + name + "\n    type: remote\n    remote:\n      url: https://" + name + ".example/mcp\n" + extra
}

func TestLoadBaseOnly(t *testing.T) {
	l := layoutDir(t, map[string]string{
		"config.yaml": "listen: \":9090\"\n" + remoteYAML("a", ""),
	})
	set := mustLoad(t, l)
	if !reflect.DeepEqual(set.Files, []string{l.BaseFile}) {
		t.Errorf("Files = %v, want [%s]", set.Files, l.BaseFile)
	}
	if set.Config.Listen != ":9090" {
		t.Errorf("Listen = %q, want :9090", set.Config.Listen)
	}
	if got := serverNames(set.Config); !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("servers = %v, want [a]", got)
	}
	want := "Defined in " + l.BaseFile + " (base)"
	if got := set.Origins["a"].String(); got != want {
		t.Errorf("Origin = %q, want %q", got, want)
	}
}

func TestLoadOrderBaseSystemUser(t *testing.T) {
	l := layoutDir(t, map[string]string{
		"config.yaml":        remoteYAML("a", ""),
		"config.d/20-y.yaml": remoteYAML("y", ""),
		"config.d/10-x.yaml": remoteYAML("x", ""),
		"user.d/z.yaml":      remoteYAML("z", ""),
	})
	set := mustLoad(t, l)
	wantFiles := []string{
		l.BaseFile,
		filepath.Join(l.SystemDir, "10-x.yaml"),
		filepath.Join(l.SystemDir, "20-y.yaml"),
		filepath.Join(l.UserDir, "z.yaml"),
	}
	if !reflect.DeepEqual(set.Files, wantFiles) {
		t.Errorf("Files = %v, want %v", set.Files, wantFiles)
	}
	if got := serverNames(set.Config); !reflect.DeepEqual(got, []string{"a", "x", "y", "z"}) {
		t.Errorf("servers = %v, want [a x y z]", got)
	}
}

func TestLoadAdminMergeRefines(t *testing.T) {
	l := layoutDir(t, map[string]string{
		"config.yaml":     remoteYAML("a", "      transport: http\n      headers: {A: \"1\"}\n"),
		"config.d/a.yaml": "servers:\n  - name: a\n    remote:\n      transport: sse\n      headers: {B: \"2\"}\n",
	})
	set := mustLoad(t, l)
	r := server(t, set.Config, "a").Remote
	if r.Transport != "sse" {
		t.Errorf("Transport = %q, want sse", r.Transport)
	}
	if !reflect.DeepEqual(r.Headers, map[string]string{"A": "1", "B": "2"}) {
		t.Errorf("Headers = %v, want {A:1 B:2}", r.Headers)
	}
	if r.URL != "https://a.example/mcp" {
		t.Errorf("URL = %q, want it kept from base", r.URL)
	}
	o := set.Origins["a"]
	want := []config.OriginFile{
		{Path: l.BaseFile, Layer: config.LayerBase},
		{Path: filepath.Join(l.SystemDir, "a.yaml"), Layer: config.LayerSystem},
	}
	if !reflect.DeepEqual(o.Files, want) {
		t.Errorf("Origin.Files = %v, want %v", o.Files, want)
	}
	if !o.Admin() {
		t.Error("Admin() = false, want true")
	}
	if o.UserFile() != "" {
		t.Errorf("UserFile() = %q, want empty", o.UserFile())
	}
	wantStr := "Defined in " + l.BaseFile + " (base), " + want[1].Path + " (system)"
	if o.String() != wantStr {
		t.Errorf("String() = %q, want %q", o.String(), wantStr)
	}
}

func TestLoadUserReplacesWholesale(t *testing.T) {
	l := layoutDir(t, map[string]string{
		"config.yaml":   remoteYAML("a", "      headers: {A: \"1\"}\n"),
		"user.d/a.yaml": remoteYAML("a", ""),
	})
	set := mustLoad(t, l)
	if h := server(t, set.Config, "a").Remote.Headers; h != nil {
		t.Errorf("Headers = %v, want nil (user replaces wholesale)", h)
	}
	user := filepath.Join(l.UserDir, "a.yaml")
	want := "Defined in " + l.BaseFile + " (base); overridden by " + user + " (user)"
	if got := set.Origins["a"].String(); got != want {
		t.Errorf("Origin = %q, want %q", got, want)
	}
	if !set.Origins["a"].Admin() || set.Origins["a"].UserFile() != user {
		t.Errorf("Origin = %+v, want admin with user file %s", set.Origins["a"], user)
	}
}

func TestLoadUserCanonicalWins(t *testing.T) {
	l := layoutDir(t, map[string]string{
		"user.d/zzz.yaml": remoteYAML("a", "      transport: sse\n") +
			"  - name: b\n    type: remote\n    remote:\n      url: https://b.example/mcp\n",
		"user.d/a.yaml": remoteYAML("a", "      transport: http\n"),
	})
	set := mustLoad(t, l)
	if got := server(t, set.Config, "a").Remote.Transport; got != "http" {
		t.Errorf("a transport = %q, want http (a.yaml wins)", got)
	}
	aFile := filepath.Join(l.UserDir, "a.yaml")
	zFile := filepath.Join(l.UserDir, "zzz.yaml")
	if got := set.Origins["a"].UserFile(); got != aFile {
		t.Errorf("Origin(a).UserFile() = %q, want %q", got, aFile)
	}
	wantFiles := []config.OriginFile{{Path: zFile, Layer: config.LayerUser}, {Path: aFile, Layer: config.LayerUser}}
	if got := set.Origins["a"].Files; !reflect.DeepEqual(got, wantFiles) {
		t.Errorf("Origin(a).Files = %v, want application order %v", got, wantFiles)
	}
	if got := set.Origins["b"].UserFile(); got != zFile {
		t.Errorf("Origin(b).UserFile() = %q, want %q", got, zFile)
	}
	if set.Origins["a"].Admin() {
		t.Error("Origin(a).Admin() = true, want false")
	}
	if want := "Defined in " + aFile + " (user)"; set.Origins["a"].String() != want {
		t.Errorf("Origin(a).String() = %q, want %q", set.Origins["a"].String(), want)
	}
	if got := serverNames(set.Config); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("servers = %v, want [a b]", got)
	}
}

func TestLoadUserTypeChangeAllowed(t *testing.T) {
	l := layoutDir(t, map[string]string{
		"config.yaml":   remoteYAML("a", ""),
		"user.d/a.yaml": "servers:\n  - name: a\n    type: command\n    command:\n      command: npx\n",
	})
	set := mustLoad(t, l)
	s := server(t, set.Config, "a")
	if s.Type != "command" || s.Remote != nil {
		t.Errorf("server = %+v, want a command server with no remote section", s)
	}
}

func TestLoadAdminTypeMismatch(t *testing.T) {
	l := layoutDir(t, map[string]string{
		"config.yaml":     remoteYAML("a", ""),
		"config.d/a.yaml": "servers:\n  - name: a\n    type: command\n    command:\n      command: npx\n",
	})
	_, err := l.Load()
	if err == nil {
		t.Fatal("want a type mismatch error")
	}
	sys := filepath.Join(l.SystemDir, "a.yaml")
	want := `config: ` + sys + `: server "a": type "command" does not match "remote" (defined in ` + l.BaseFile + `)`
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestLoadDuplicateInOneFile(t *testing.T) {
	l := layoutDir(t, map[string]string{
		"config.d/x.yaml": remoteYAML("a", "") + "  - name: a\n    type: remote\n",
	})
	_, err := l.Load()
	want := `config: ` + filepath.Join(l.SystemDir, "x.yaml") + `: duplicate server "a"`
	if err == nil || err.Error() != want {
		t.Errorf("error = %v, want %q", err, want)
	}
}

func TestLoadGlobalKeyInDropIn(t *testing.T) {
	l := layoutDir(t, map[string]string{"config.d/x.yaml": "listen: :1\n"})
	_, err := l.Load()
	if err == nil {
		t.Fatal("want an error for a global key in a drop-in")
	}
	file := filepath.Join(l.SystemDir, "x.yaml")
	prefix := "config: " + file + ": parse: "
	if !strings.HasPrefix(err.Error(), prefix) || !strings.Contains(err.Error(), "listen") {
		t.Errorf("error = %q, want prefix %q and mention of listen", err, prefix)
	}
	if strings.Count(err.Error(), "config:") != 1 {
		t.Errorf("error = %q, want a single config: prefix", err)
	}
	if !strings.Contains(err.Error(), "config.dropIn") {
		t.Errorf("error = %q, want the named type config.dropIn", err)
	}
}

func TestLoadEmptyDropIn(t *testing.T) {
	l := layoutDir(t, map[string]string{"config.d/x.yaml": "  \n"})
	_, err := l.Load()
	want := "config: " + filepath.Join(l.SystemDir, "x.yaml") + ": file is empty"
	if err == nil || err.Error() != want {
		t.Errorf("error = %v, want %q", err, want)
	}
}

func TestLoadEmptyBase(t *testing.T) {
	l := layoutDir(t, map[string]string{"config.yaml": ""})
	_, err := l.Load()
	want := "config: " + l.BaseFile + ": file is empty"
	if err == nil || err.Error() != want {
		t.Errorf("error = %v, want %q", err, want)
	}
}

func TestLoadBaseParseError(t *testing.T) {
	l := layoutDir(t, map[string]string{"config.yaml": "lisen: x\n"})
	_, err := l.Load()
	prefix := "config: " + l.BaseFile + ": parse: "
	if err == nil || !strings.HasPrefix(err.Error(), prefix) || !strings.Contains(err.Error(), "lisen") {
		t.Errorf("error = %v, want prefix %q naming lisen", err, prefix)
	}
}

func TestLoadBaseIsDirectory(t *testing.T) {
	l := layoutDir(t, map[string]string{"config.yaml/keep": "x"})
	_, err := l.Load()
	if err == nil || !strings.HasPrefix(err.Error(), "config: "+l.BaseFile+": ") {
		t.Errorf("error = %v, want an error naming %s", err, l.BaseFile)
	}
}

func TestLoadMissingBaseIsFine(t *testing.T) {
	l := layoutDir(t, map[string]string{"config.d/x.yaml": remoteYAML("x", "")})
	set := mustLoad(t, l)
	if set.Config.Listen != ":8080" {
		t.Errorf("Listen = %q, want default :8080", set.Config.Listen)
	}
	if len(set.Config.Servers) != 1 || set.Config.Servers[0].Remote.Transport != "auto" {
		t.Errorf("servers = %+v, want one defaulted server", set.Config.Servers)
	}
	if !reflect.DeepEqual(set.Files, []string{filepath.Join(l.SystemDir, "x.yaml")}) {
		t.Errorf("Files = %v, want only the drop-in", set.Files)
	}
}

func TestLoadNothingFound(t *testing.T) {
	l := layoutDir(t, nil)
	l.Fallback = "/legacy/config.yaml"
	set := mustLoad(t, l)
	if len(set.Files) != 0 {
		t.Errorf("Files = %v, want empty", set.Files)
	}
	if set.Config.Listen != ":8080" || len(set.Config.Servers) != 0 {
		t.Errorf("Config = %+v, want defaults and no servers", set.Config)
	}
	if !reflect.DeepEqual(set.Searched, l.Searched) {
		t.Errorf("Searched = %v, want %v", set.Searched, l.Searched)
	}
	if set.Fallback != l.Fallback {
		t.Errorf("Fallback = %q, want %q", set.Fallback, l.Fallback)
	}
}

func TestLoadSymlinkedDropIn(t *testing.T) {
	l := layoutDir(t, map[string]string{"config.d/.keep": ""})
	target := filepath.Join(t.TempDir(), "real.yaml")
	if err := os.WriteFile(target, []byte(remoteYAML("x", "")), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(l.SystemDir, "x.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	set := mustLoad(t, l)
	if !reflect.DeepEqual(set.Files, []string{link}) {
		t.Errorf("Files = %v, want [%s]", set.Files, link)
	}
	if got := serverNames(set.Config); !reflect.DeepEqual(got, []string{"x"}) {
		t.Errorf("servers = %v, want [x]", got)
	}
}

func TestLoadRawKeepsPlaceholders(t *testing.T) {
	l := layoutDir(t, map[string]string{
		"config.yaml": remoteYAML("a", "      headers: {Authorization: \"${TOK}\"}\n"),
	})
	t.Setenv("TOK", "")
	if err := os.Unsetenv("TOK"); err != nil {
		t.Fatal(err)
	}

	raw, err := l.LoadRaw()
	if err != nil {
		t.Fatalf("LoadRaw: %v", err)
	}
	if got := server(t, raw.Config, "a").Remote.Headers["Authorization"]; got != "${TOK}" {
		t.Errorf("raw header = %q, want ${TOK}", got)
	}

	if _, err := l.Load(); err == nil || !strings.Contains(err.Error(), "${TOK} is not set") {
		t.Errorf("Load with TOK unset: error = %v, want ${TOK} is not set", err)
	}

	t.Setenv("TOK", "1")
	set := mustLoad(t, l)
	if got := server(t, set.Config, "a").Remote.Headers["Authorization"]; got != "1" {
		t.Errorf("header = %q, want 1", got)
	}
}

func TestLoadSocketEnvOverride(t *testing.T) {
	t.Setenv("MCP_MACHINE_PODMAN_SOCKET", "unix:///tmp/p.sock")
	l := layoutDir(t, map[string]string{"config.yaml": "servers: []\n"})
	set := mustLoad(t, l)
	if set.Config.Podman.Socket != "unix:///tmp/p.sock" {
		t.Errorf("Load socket = %q, want env override", set.Config.Podman.Socket)
	}
	raw, err := l.LoadRaw()
	if err != nil {
		t.Fatalf("LoadRaw: %v", err)
	}
	if raw.Config.Podman.Socket != "unix:///run/podman/podman.sock" {
		t.Errorf("LoadRaw socket = %q, want the default", raw.Config.Podman.Socket)
	}
}

func TestLoadFileWrapperUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := "listen: \":9090\"\n" + remoteYAML("a", "      headers: {A: \"1\"}\n") +
		"  - name: c\n    type: command\n    command:\n      command: npx\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	set, err := config.Layout{BaseFile: path}.Load()
	if err != nil {
		t.Fatalf("Layout.Load: %v", err)
	}
	if !reflect.DeepEqual(c, set.Config) {
		t.Errorf("config.Load = %+v, want %+v", c, set.Config)
	}
}

func TestLayerString(t *testing.T) {
	for l, want := range map[config.Layer]string{
		config.LayerBase: "base", config.LayerSystem: "system", config.LayerUser: "user",
	} {
		if l.String() != want {
			t.Errorf("Layer(%d).String() = %q, want %q", int(l), l.String(), want)
		}
	}
}
