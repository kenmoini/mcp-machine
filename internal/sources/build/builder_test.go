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
)

const testSHA = "0123456789abcdef0123456789abcdef01234567"

// fakeCloner copies testdata/repo into dir and returns a fixed sha.
type fakeCloner struct {
	sha   string
	calls int
	err   error
}

func (f *fakeCloner) Clone(_ context.Context, _, _, dir string, _ *config.GitAuth) (string, error) {
	f.calls++
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
