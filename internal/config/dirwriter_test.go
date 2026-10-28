package config_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kenmoini/mcp-machine/internal/config"
)

// dirWriter returns a DirWriter over l's user drop-in directory.
func dirWriter(l config.Layout) config.DirWriter {
	return config.DirWriter{Dir: l.UserDir, Layout: l}
}

func remoteSpec(name string) config.ServerSpec {
	return config.ServerSpec{
		Name:   name,
		Type:   "remote",
		Remote: &config.RemoteSpec{URL: "https://" + name + ".example/mcp", Transport: "http"},
	}
}

func readBytes(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p) //nolint:gosec // G304: test-owned temp path
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustLoadRaw(t *testing.T, l config.Layout) *config.Set {
	t.Helper()
	set, err := l.LoadRaw()
	if err != nil {
		t.Fatalf("LoadRaw: %v", err)
	}
	return set
}

// decodeServers strictly decodes a drop-in file written by DirWriter.
func decodeServers(t *testing.T, p string) []config.ServerSpec {
	t.Helper()
	var doc struct {
		Servers []config.ServerSpec `yaml:"servers"`
	}
	if err := yaml.Unmarshal(readBytes(t, p), &doc); err != nil {
		t.Fatalf("decode %s: %v", p, err)
	}
	return doc.Servers
}

// dirEntries returns the names in dir, or nil when it does not exist.
func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestDirWriterAddWritesOneFile(t *testing.T) {
	l := layoutDir(t, map[string]string{"config.yaml": remoteYAML("alpha", "")})
	before := readBytes(t, l.BaseFile)

	if err := dirWriter(l).AddServer(remoteSpec("gamma")); err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	p := filepath.Join(l.UserDir, "gamma.yaml")
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", fi.Mode().Perm())
	}
	got := decodeServers(t, p)
	if len(got) != 1 || !reflect.DeepEqual(got[0], remoteSpec("gamma")) {
		t.Errorf("file servers = %+v, want exactly gamma", got)
	}
	if after := readBytes(t, l.BaseFile); string(after) != string(before) {
		t.Errorf("base file changed:\n%s", after)
	}
	if names := dirEntries(t, l.UserDir); !reflect.DeepEqual(names, []string{"gamma.yaml"}) {
		t.Errorf("user dir = %v, want [gamma.yaml]", names)
	}
}

func TestDirWriterAddRefusesExistingName(t *testing.T) {
	l := layoutDir(t, map[string]string{"config.yaml": remoteYAML("alpha", "")})
	err := dirWriter(l).AddServer(remoteSpec("alpha"))
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v, want already exists", err)
	}
	if names := dirEntries(t, l.UserDir); len(names) != 0 {
		t.Errorf("user dir = %v, want nothing written", names)
	}
}

func TestDirWriterAddRefusesExistingFile(t *testing.T) {
	l := layoutDir(t, map[string]string{"user.d/gamma.yaml": remoteYAML("delta", "")})
	p := filepath.Join(l.UserDir, "gamma.yaml")
	before := readBytes(t, p)

	err := dirWriter(l).AddServer(remoteSpec("gamma"))
	if err == nil || !strings.Contains(err.Error(), p) {
		t.Fatalf("err = %v, want it to name %s", err, p)
	}
	if after := readBytes(t, p); string(after) != string(before) {
		t.Errorf("file changed:\n%s", after)
	}
}

func TestDirWriterAddKeepsPlaceholders(t *testing.T) {
	// The base holds a placeholder nothing in the environment sets: the
	// writer validates against the raw set, so it must not need the value.
	l := layoutDir(t, map[string]string{
		"config.yaml": remoteYAML("alpha", "      headers:\n        X-Base: \"${DIRWRITER_UNSET_BASE}\"\n"),
	})
	s := remoteSpec("gamma")
	s.Remote.Headers = map[string]string{"Authorization": "Bearer ${TOK}"}

	if err := dirWriter(l).AddServer(s); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	p := filepath.Join(l.UserDir, "gamma.yaml")
	if b := readBytes(t, p); !strings.Contains(string(b), "${TOK}") {
		t.Errorf("file lost the placeholder:\n%s", b)
	}
	got := server(t, mustLoadRaw(t, l).Config, "gamma")
	if h := got.Remote.Headers["Authorization"]; h != "Bearer ${TOK}" {
		t.Errorf("Authorization = %q, want the placeholder", h)
	}
}

func TestDirWriterAddValidatesMergedSet(t *testing.T) {
	l := layoutDir(t, map[string]string{"config.yaml": remoteYAML("alpha", "")})
	err := dirWriter(l).AddServer(remoteSpec("Bad_Name"))
	if err == nil || !strings.Contains(err.Error(), "invalid name") {
		t.Fatalf("err = %v, want a validation error", err)
	}
	if names := dirEntries(t, l.UserDir); len(names) != 0 {
		t.Errorf("user dir = %v, want nothing written", names)
	}
}

func TestDirWriterUpdateOverridesAdmin(t *testing.T) {
	l := layoutDir(t, map[string]string{"config.yaml": remoteYAML("alpha", "")})
	before := readBytes(t, l.BaseFile)

	s := remoteSpec("alpha")
	s.Remote.Transport = "sse"
	if err := dirWriter(l).UpdateServer("alpha", s); err != nil {
		t.Fatalf("UpdateServer: %v", err)
	}

	p := filepath.Join(l.UserDir, "alpha.yaml")
	if got := decodeServers(t, p); len(got) != 1 || got[0].Name != "alpha" {
		t.Errorf("file servers = %+v, want exactly alpha", got)
	}
	set := mustLoadRaw(t, l)
	if tr := server(t, set.Config, "alpha").Remote.Transport; tr != "sse" {
		t.Errorf("transport = %q, want sse", tr)
	}
	o := set.Origins["alpha"]
	if !strings.Contains(o.String(), "overridden by "+p) {
		t.Errorf("origin = %q, want it overridden by %s", o.String(), p)
	}
	if after := readBytes(t, l.BaseFile); string(after) != string(before) {
		t.Errorf("base file changed:\n%s", after)
	}
}

func TestDirWriterUpdateReplacesWholesale(t *testing.T) {
	l := layoutDir(t, map[string]string{
		"config.yaml": remoteYAML("alpha", "      headers:\n        A: \"1\"\n"),
	})
	if h := server(t, mustLoadRaw(t, l).Config, "alpha").Remote.Headers; h["A"] != "1" {
		t.Fatalf("precondition: headers = %v", h)
	}
	if err := dirWriter(l).UpdateServer("alpha", remoteSpec("alpha")); err != nil {
		t.Fatalf("UpdateServer: %v", err)
	}
	if h := server(t, mustLoadRaw(t, l).Config, "alpha").Remote.Headers; h != nil {
		t.Errorf("headers = %v, want nil", h)
	}
}

func TestDirWriterUpdateRefusesForeignOwnFile(t *testing.T) {
	both := "servers:\n" +
		"  - {name: alpha, type: remote, remote: {url: https://alpha.example/mcp}}\n" +
		"  - {name: beta, type: remote, remote: {url: https://beta.example/mcp}}\n"
	l := layoutDir(t, map[string]string{"user.d/alpha.yaml": both})
	p := filepath.Join(l.UserDir, "alpha.yaml")

	s := remoteSpec("alpha")
	s.Remote.Transport = "sse"
	err := dirWriter(l).UpdateServer("alpha", s)
	if !errors.Is(err, config.ErrForeignFile) || !strings.Contains(err.Error(), p) {
		t.Fatalf("err = %v, want ErrForeignFile naming %s", err, p)
	}
	if b := readBytes(t, p); string(b) != both {
		t.Errorf("file changed:\n%s", b)
	}
}

func TestDirWriterUpdateNameChange(t *testing.T) {
	l := layoutDir(t, map[string]string{"config.yaml": remoteYAML("alpha", "")})
	err := dirWriter(l).UpdateServer("alpha", remoteSpec("beta"))
	if err == nil || !strings.Contains(err.Error(), "name cannot change") {
		t.Fatalf("err = %v, want name cannot change", err)
	}
	if names := dirEntries(t, l.UserDir); len(names) != 0 {
		t.Errorf("user dir = %v, want nothing written", names)
	}
}

func TestDirWriterUpdateNotFound(t *testing.T) {
	l := layoutDir(t, map[string]string{"config.yaml": remoteYAML("alpha", "")})
	err := dirWriter(l).UpdateServer("nope", remoteSpec("nope"))
	if err == nil || err.Error() != `config: server "nope" not found` {
		t.Fatalf("err = %v, want not found", err)
	}
	if names := dirEntries(t, l.UserDir); len(names) != 0 {
		t.Errorf("user dir = %v, want nothing written", names)
	}
}

func TestDirWriterDeleteUserOnly(t *testing.T) {
	l := layoutDir(t, map[string]string{"user.d/gamma.yaml": remoteYAML("gamma", "")})
	if err := dirWriter(l).DeleteServer("gamma"); err != nil {
		t.Fatalf("DeleteServer: %v", err)
	}
	if _, err := os.Stat(filepath.Join(l.UserDir, "gamma.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat = %v, want file gone", err)
	}
	if _, ok := mustLoadRaw(t, l).Origins["gamma"]; ok {
		t.Error("gamma still loaded")
	}
}

func TestDirWriterDeleteRevertsOverride(t *testing.T) {
	l := layoutDir(t, map[string]string{
		"config.yaml":       remoteYAML("alpha", ""),
		"user.d/alpha.yaml": remoteYAML("alpha", "      transport: sse\n"),
	})
	before := readBytes(t, l.BaseFile)

	if err := dirWriter(l).DeleteServer("alpha"); err != nil {
		t.Fatalf("DeleteServer: %v", err)
	}
	if _, err := os.Stat(filepath.Join(l.UserDir, "alpha.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat = %v, want override gone", err)
	}
	set := mustLoadRaw(t, l)
	if tr := server(t, set.Config, "alpha").Remote.Transport; tr != "auto" {
		t.Errorf("transport = %q, want the base default auto", tr)
	}
	o := set.Origins["alpha"]
	if !o.Admin() || o.UserFile() != "" {
		t.Errorf("origin = %+v, want admin only", o)
	}
	if after := readBytes(t, l.BaseFile); string(after) != string(before) {
		t.Errorf("base file changed:\n%s", after)
	}
}

func TestDirWriterDeleteAdminRefused(t *testing.T) {
	l := layoutDir(t, map[string]string{
		"config.yaml":        remoteYAML("alpha", ""),
		"config.d/beta.yaml": remoteYAML("beta", ""),
	})
	base := readBytes(t, l.BaseFile)
	sys := readBytes(t, filepath.Join(l.SystemDir, "beta.yaml"))

	for _, name := range []string{"alpha", "beta"} {
		if err := dirWriter(l).DeleteServer(name); !errors.Is(err, config.ErrAdminOwned) {
			t.Errorf("DeleteServer(%s) = %v, want ErrAdminOwned", name, err)
		}
	}
	if after := readBytes(t, l.BaseFile); string(after) != string(base) {
		t.Errorf("base file changed:\n%s", after)
	}
	if after := readBytes(t, filepath.Join(l.SystemDir, "beta.yaml")); string(after) != string(sys) {
		t.Errorf("system file changed:\n%s", after)
	}
	if err := dirWriter(l).DeleteServer("nope"); err == nil || err.Error() != `config: server "nope" not found` {
		t.Errorf("DeleteServer(nope) = %v, want not found", err)
	}
}

func TestDirWriterDeleteForeignRefused(t *testing.T) {
	l := layoutDir(t, map[string]string{"user.d/zzz.yaml": remoteYAML("gamma", "")})
	p := filepath.Join(l.UserDir, "zzz.yaml")
	err := dirWriter(l).DeleteServer("gamma")
	if !errors.Is(err, config.ErrForeignFile) || !strings.Contains(err.Error(), "zzz.yaml") {
		t.Fatalf("err = %v, want ErrForeignFile naming zzz.yaml", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("stat = %v, want file still there", err)
	}
}

// A canonical <name>.yaml that also defines other servers is not the UI's to
// delete: removing it would take the other servers with it.
func TestDirWriterDeleteSharedCanonicalRefused(t *testing.T) {
	both := "servers:\n" +
		"  - {name: gamma, type: remote, remote: {url: https://gamma.example/mcp}}\n" +
		"  - {name: delta, type: remote, remote: {url: https://delta.example/mcp}}\n"
	l := layoutDir(t, map[string]string{"user.d/gamma.yaml": both})
	p := filepath.Join(l.UserDir, "gamma.yaml")
	err := dirWriter(l).DeleteServer("gamma")
	if !errors.Is(err, config.ErrForeignFile) || !strings.Contains(err.Error(), p) {
		t.Fatalf("err = %v, want ErrForeignFile naming %s", err, p)
	}
	if b := readBytes(t, p); string(b) != both {
		t.Errorf("file changed:\n%s", b)
	}
}

func TestDirWriterConcurrentAdds(t *testing.T) {
	l := layoutDir(t, map[string]string{"config.yaml": remoteYAML("alpha", "")})
	const n = 10

	// Distinct names: every add lands.
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			// A fresh DirWriter value per goroutine: the lock is process-wide.
			errs <- dirWriter(l).AddServer(remoteSpec(fmt.Sprintf("srv-%02d", i)))
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("AddServer: %v", err)
		}
	}
	set := mustLoadRaw(t, l)
	for i := range n {
		name := fmt.Sprintf("srv-%02d", i)
		if _, ok := set.Origins[name]; !ok {
			t.Errorf("%s missing from LoadRaw", name)
		}
	}
	if names := dirEntries(t, l.UserDir); len(names) != n {
		t.Errorf("user dir = %v, want %d files and no temp files", names, n)
	}

	// The same name: the check and the write are one critical section, so
	// exactly one add wins.
	errs = make(chan error, n)
	for range n {
		wg.Go(func() { errs <- dirWriter(l).AddServer(remoteSpec("same")) })
	}
	wg.Wait()
	close(errs)
	var ok int
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case !strings.Contains(err.Error(), "already exists"):
			t.Errorf("AddServer(same) = %v, want already exists", err)
		}
	}
	if ok != 1 {
		t.Errorf("%d concurrent adds of the same name succeeded, want 1", ok)
	}
}

func TestDirWriterCreatesDir(t *testing.T) {
	l := layoutDir(t, map[string]string{"config.yaml": remoteYAML("alpha", "")})
	if _, err := os.Stat(l.UserDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("precondition: user dir stat = %v", err)
	}
	if err := dirWriter(l).AddServer(remoteSpec("gamma")); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	fi, err := os.Stat(l.UserDir)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() || fi.Mode().Perm() != 0o750 {
		t.Errorf("user dir mode = %v, want a 0750 directory", fi.Mode())
	}
}
