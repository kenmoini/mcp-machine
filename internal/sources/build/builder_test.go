package build

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/runtime"
	"github.com/kenmoini/mcp-machine/internal/runtime/fake"
	"github.com/kenmoini/mcp-machine/internal/sources"
	"github.com/kenmoini/mcp-machine/internal/store"
	"github.com/kenmoini/mcp-machine/internal/vault"
)

const testSHA = "0123456789abcdef0123456789abcdef01234567"

// fakeCloner copies testdata/repo into dir and returns a fixed sha.
type fakeCloner struct {
	sha   string
	calls int
	err   error
	auth  *config.GitAuth // the last Clone's credentials
	// after, if set, runs on the cloned tree (to plant files or symlinks).
	after func(dir string) error
}

func (f *fakeCloner) Clone(_ context.Context, _, _, dir string, auth *config.GitAuth) (string, error) {
	f.calls++
	f.auth = auth
	if f.err != nil {
		return "", f.err
	}
	src := filepath.Join("testdata", "repo")
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o644)
	})
	if err == nil && f.after != nil {
		err = f.after(dir)
	}
	return f.sha, err
}

func testPlan() sources.BuildPlan {
	return sources.BuildPlan{
		Name:          "svc",
		Repo:          "https://example.com/r.git",
		Ref:           "main",
		Subdir:        "server",
		Containerfile: "Containerfile",
		Args:          map[string]string{"A": "1"},
		Auth:          &config.GitAuth{Token: "t"},
		RegistryAuth:  &runtime.RegistryAuth{Username: "u", Password: "p"},
	}
}

func TestBuilderClonesAndBuilds(t *testing.T) {
	eng := fake.New()
	b := &Builder{Workspace: t.TempDir(), Engine: eng, Cloner: &fakeCloner{sha: testSHA}}
	tag, err := b.Build(context.Background(), testPlan(), false)
	if err != nil {
		t.Fatal(err)
	}
	if tag != "localhost/mcp-machine/svc:0123456" {
		t.Fatalf("tag = %q", tag)
	}
	builds := eng.Builds()
	if len(builds) != 1 {
		t.Fatalf("builds = %d", len(builds))
	}
	req := builds[0]
	if !strings.HasSuffix(filepath.ToSlash(req.ContextDir), "/svc/src/server") {
		t.Fatalf("ContextDir = %q", req.ContextDir)
	}
	if _, err := os.Stat(filepath.Join(req.ContextDir, "Containerfile")); err != nil {
		t.Fatalf("context missing Containerfile: %v", err)
	}
	if req.Containerfile != "Containerfile" || req.Tag != tag {
		t.Fatalf("req = %+v", req)
	}
	if req.Args["A"] != "1" {
		t.Fatalf("args = %v", req.Args)
	}
	if req.Labels[store.LabelServer] != "svc" || req.Labels["io.mcp-machine/commit"] != testSHA {
		t.Fatalf("labels = %v", req.Labels)
	}
	if req.Auth == nil || req.Auth.Username != "u" || req.Auth.Password != "p" {
		t.Fatalf("auth = %+v", req.Auth)
	}
}

func TestBuilderSkipsWhenImageExists(t *testing.T) {
	eng := fake.New()
	b := &Builder{Workspace: t.TempDir(), Engine: eng, Cloner: &fakeCloner{sha: testSHA}}
	ctx := context.Background()
	tag1, err := b.Build(ctx, testPlan(), false)
	if err != nil {
		t.Fatal(err)
	}
	tag2, err := b.Build(ctx, testPlan(), false)
	if err != nil {
		t.Fatal(err)
	}
	if tag1 != tag2 || len(eng.Builds()) != 1 {
		t.Fatalf("tags %q/%q builds %d; want cached", tag1, tag2, len(eng.Builds()))
	}
	if _, err := b.Build(ctx, testPlan(), true); err != nil {
		t.Fatal(err)
	}
	if len(eng.Builds()) != 2 {
		t.Fatalf("builds = %d after force; want 2", len(eng.Builds()))
	}
}

func TestBuilderRecreatesSrcDir(t *testing.T) {
	ws := t.TempDir()
	stale := filepath.Join(ws, "svc", "src", "stale.txt")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := &Builder{Workspace: ws, Engine: fake.New(), Cloner: &fakeCloner{sha: testSHA}}
	if _, err := b.Build(context.Background(), testPlan(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stale file still present: %v", err)
	}
}

func TestBuilderCloneError(t *testing.T) {
	eng := fake.New()
	b := &Builder{Workspace: t.TempDir(), Engine: eng, Cloner: &fakeCloner{err: errors.New("boom")}}
	if _, err := b.Build(context.Background(), testPlan(), false); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	if len(eng.Builds()) != 0 {
		t.Fatal("built despite clone error")
	}
}

func TestBuilderRejectsShortSHA(t *testing.T) {
	b := &Builder{Workspace: t.TempDir(), Engine: fake.New(), Cloner: &fakeCloner{sha: "abc"}}
	if _, err := b.Build(context.Background(), testPlan(), false); err == nil {
		t.Fatal("want error for short sha")
	}
}

func withContainerfile(content string) func(string) error {
	return func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "server", "Containerfile"), []byte(content), 0o644)
	}
}

func buildAuth(t *testing.T, containerfile string, regs []config.Registry) *runtime.RegistryAuth {
	t.Helper()
	eng := fake.New()
	b := &Builder{
		Workspace: t.TempDir(), Engine: eng, Registries: regs,
		Cloner: &fakeCloner{sha: testSHA, after: withContainerfile(containerfile)},
	}
	plan := testPlan()
	plan.RegistryAuth = nil
	if _, err := b.Build(context.Background(), plan, false); err != nil {
		t.Fatal(err)
	}
	return eng.Builds()[0].Auth
}

func TestBuilderRegistryAuthFromBaseImage(t *testing.T) {
	ghcr := []config.Registry{{Registry: "ghcr.io", Username: "gu", Password: "gp"}}
	hub := []config.Registry{{Registry: "docker.io", Username: "hu", Password: "hp"}}
	tests := []struct {
		name, containerfile string
		regs                []config.Registry
		want                *runtime.RegistryAuth
	}{
		{"matching registry", "FROM ghcr.io/kenmoini/base:1\nRUN true\n", ghcr, &runtime.RegistryAuth{Username: "gu", Password: "gp"}},
		{"no matching registry", "FROM docker.io/library/node:22\n", ghcr, nil},
		{"multi-stage picks first image", "FROM ghcr.io/o/a AS build\nRUN x\nFROM build\n", ghcr, &runtime.RegistryAuth{Username: "gu", Password: "gp"}},
		{"alias is not an image", "FROM scratch AS base\nFROM base AS two\nFROM ghcr.io/o/b\n", ghcr, &runtime.RegistryAuth{Username: "gu", Password: "gp"}},
		{"case and platform flag", "# c\nfrom --platform=linux/amd64 ghcr.io/o/a:1\n", ghcr, &runtime.RegistryAuth{Username: "gu", Password: "gp"}},
		{"arg expanded from build args", "ARG A\nFROM ghcr.io/o/${A}\n", ghcr, &runtime.RegistryAuth{Username: "gu", Password: "gp"}},
		{"docker.io entry does not leak to ghcr base", "FROM ghcr.io/o/a\n", hub, nil},
		{"docker.io entry applies to hub base", "FROM node:22\n", hub, &runtime.RegistryAuth{Username: "hu", Password: "hp"}},
		{"scratch only", "FROM scratch\n", hub, nil},
		{"no from", "RUN true\n", hub, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildAuth(t, tc.containerfile, tc.regs)
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Fatalf("auth = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestBuilderRejectsBadSubdir(t *testing.T) {
	for _, sub := range []string{"../x", "..", "a/../../x", "/etc"} {
		eng := fake.New()
		cl := &fakeCloner{sha: testSHA}
		b := &Builder{Workspace: t.TempDir(), Engine: eng, Cloner: cl}
		plan := testPlan()
		plan.Subdir = sub
		if _, err := b.Build(context.Background(), plan, false); err == nil {
			t.Errorf("subdir %q: want error", sub)
		}
		if len(eng.Builds()) != 0 {
			t.Errorf("subdir %q: built", sub)
		}
	}
}

func TestBuilderRejectsSymlinkEscape(t *testing.T) {
	outside := t.TempDir()
	eng := fake.New()
	cl := &fakeCloner{sha: testSHA, after: func(dir string) error {
		return os.Symlink(outside, filepath.Join(dir, "escape"))
	}}
	b := &Builder{Workspace: t.TempDir(), Engine: eng, Cloner: cl}
	plan := testPlan()
	plan.Subdir = "escape"
	if _, err := b.Build(context.Background(), plan, false); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("err = %v", err)
	}
	if len(eng.Builds()) != 0 {
		t.Fatal("built with escaping context")
	}
}

func TestBuilderAcceptsInRepoSymlinkAndDotSubdir(t *testing.T) {
	eng := fake.New()
	cl := &fakeCloner{sha: testSHA, after: func(dir string) error {
		return os.Symlink("server", filepath.Join(dir, "alias"))
	}}
	b := &Builder{Workspace: t.TempDir(), Engine: eng, Cloner: cl}
	plan := testPlan()
	plan.Subdir = "alias"
	if _, err := b.Build(context.Background(), plan, false); err != nil {
		t.Fatal(err)
	}
	plan.Subdir = ""
	plan.Containerfile = "server/Containerfile"
	if _, err := b.Build(context.Background(), plan, true); err != nil {
		t.Fatal(err)
	}
}

func TestBuilderValidatesWorkspaceAndName(t *testing.T) {
	cases := map[string]struct {
		ws   string
		name string
	}{
		"empty workspace":    {"", "svc"},
		"relative workspace": {"rel/dir", "svc"},
		"empty name":         {t.TempDir(), ""},
		"dotdot name":        {t.TempDir(), ".."},
		"traversal name":     {t.TempDir(), "../evil"},
		"slash name":         {t.TempDir(), "a/b"},
		"uppercase name":     {t.TempDir(), "Svc"},
	}
	for label, tc := range cases {
		t.Run(label, func(t *testing.T) {
			eng := fake.New()
			cl := &fakeCloner{sha: testSHA}
			b := &Builder{Workspace: tc.ws, Engine: eng, Cloner: cl}
			plan := testPlan()
			plan.Name = tc.name
			if _, err := b.Build(context.Background(), plan, false); err == nil {
				t.Fatal("want error")
			}
			if cl.calls != 0 || len(eng.Builds()) != 0 {
				t.Fatal("cloned or built despite invalid input")
			}
		})
	}
}

func TestBuilderDoesNotDeleteOutsideWorkspace(t *testing.T) {
	parent := t.TempDir()
	victim := filepath.Join(parent, "victim")
	if err := os.MkdirAll(filepath.Join(victim, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	b := &Builder{Workspace: filepath.Join(parent, "ws"), Engine: fake.New(), Cloner: &fakeCloner{sha: testSHA}}
	plan := testPlan()
	plan.Name = "../../victim"
	if _, err := b.Build(context.Background(), plan, false); err == nil {
		t.Fatal("want error")
	}
	if !exists(filepath.Join(victim, "src")) {
		t.Fatal("directory outside workspace was removed")
	}
}

func TestBuildResolvesVaultCredentials(t *testing.T) {
	fv := &vault.Fake{}
	fv.Set("kv/data/git", "token", "ghp_real")
	fv.Set("kv/data/reg", "pass", "reg_real")
	plan := testPlan()
	plan.Auth = &config.GitAuth{Token: "${vault:kv/data/git#token}"}
	plan.RegistryAuth = &runtime.RegistryAuth{Username: "u", Password: "${vault:kv/data/reg#pass}"}

	eng := fake.New()
	cl := &fakeCloner{sha: testSHA}
	b := &Builder{Workspace: t.TempDir(), Engine: eng, Cloner: cl, Secrets: fv}
	if _, err := b.Build(context.Background(), plan, false); err != nil {
		t.Fatal(err)
	}
	if cl.auth == nil || cl.auth.Token != "ghp_real" {
		t.Errorf("clone auth = %+v, want the resolved token", cl.auth)
	}
	if plan.Auth.Token != "${vault:kv/data/git#token}" {
		t.Error("the plan's template was overwritten")
	}
	builds := eng.Builds()
	if len(builds) != 1 || builds[0].Auth == nil || builds[0].Auth.Password != "reg_real" || builds[0].Auth.Username != "u" {
		t.Errorf("build auth = %+v, want the resolved password", builds)
	}

	// Without Vault a reference is refused before cloning.
	cl2 := &fakeCloner{sha: testSHA}
	b2 := &Builder{Workspace: t.TempDir(), Engine: fake.New(), Cloner: cl2}
	_, err := b2.Build(context.Background(), plan, false)
	if err == nil || !strings.Contains(err.Error(), "git credentials: vault is not configured") {
		t.Errorf("err = %v", err)
	}
	if cl2.calls != 0 {
		t.Error("cloned without credentials")
	}

	// A plan without references never touches Vault.
	b3 := &Builder{Workspace: t.TempDir(), Engine: fake.New(), Cloner: &fakeCloner{sha: testSHA}}
	if _, err := b3.Build(context.Background(), testPlan(), false); err != nil {
		t.Fatal(err)
	}
}
