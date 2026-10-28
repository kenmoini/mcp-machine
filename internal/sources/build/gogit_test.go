package build

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"

	"github.com/kenmoini/mcp-machine/internal/config"
)

// makeRepo creates a work-tree repo with two commits on main (a.txt, then
// b.txt) and tag v1 on the first. It returns the repo path and both shas.
func makeRepo(t *testing.T) (dir, first, second string) {
	t.Helper()
	dir = t.TempDir()
	repo, err := git.PlainInitWithOptions(dir, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.Main},
	})
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "t", Email: "t@example.com", When: time.Unix(1700000000, 0)}
	commit := func(file string) plumbing.Hash {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, file), []byte(file), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := wt.Add(file); err != nil {
			t.Fatal(err)
		}
		h, err := wt.Commit("add "+file, &git.CommitOptions{Author: sig, Committer: sig})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	h1 := commit("a.txt")
	if _, err := repo.CreateTag("v1", h1, nil); err != nil {
		t.Fatal(err)
	}
	h2 := commit("b.txt")
	return dir, h1.String(), h2.String()
}

// hermeticGit prepares the environment for go-git's file:// transport, which
// runs the local git-upload-pack binary: it skips the test when git is not
// installed and keeps the user's git configuration out of the picture.
func hermeticGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git-upload-pack"); err != nil {
		t.Skip("git-upload-pack not installed; file:// transport unavailable")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func TestGoGitCloneAtRef(t *testing.T) {
	repoDir, first, second := makeRepo(t)
	hermeticGit(t)
	url := "file://" + repoDir
	tests := []struct {
		name, ref, wantSHA string
		wantB              bool
	}{
		{"branch", "main", second, true},
		{"default branch", "", second, true},
		{"tag", "v1", first, false},
		{"sha", first, first, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "src")
			sha, err := GoGitCloner{}.Clone(context.Background(), url, tc.ref, dst, nil)
			if err != nil {
				t.Fatal(err)
			}
			if sha != tc.wantSHA {
				t.Fatalf("sha = %s, want %s", sha, tc.wantSHA)
			}
			if !exists(filepath.Join(dst, "a.txt")) {
				t.Fatal("a.txt missing")
			}
			if got := exists(filepath.Join(dst, "b.txt")); got != tc.wantB {
				t.Fatalf("b.txt present = %v, want %v", got, tc.wantB)
			}
		})
	}
}

func TestGoGitCloneUnknownRef(t *testing.T) {
	repoDir, _, _ := makeRepo(t)
	hermeticGit(t)
	dst := filepath.Join(t.TempDir(), "src")
	if _, err := (GoGitCloner{}).Clone(context.Background(), "file://"+repoDir, "nope", dst, nil); err == nil {
		t.Fatal("want error for unknown ref")
	}
}

func TestGoGitAuthMethod(t *testing.T) {
	m, err := AuthMethod("https://github.com/o/r.git", &config.GitAuth{Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	ba, ok := m.(*githttp.BasicAuth)
	if !ok || ba.Password != "tok" || ba.Username != "x-access-token" {
		t.Fatalf("auth = %#v", m)
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(blk), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err = AuthMethod("git@github.com:o/r.git", &config.GitAuth{SSHKeyFile: keyFile})
	if err != nil {
		t.Fatal(err)
	}
	pk, ok := m.(*gitssh.PublicKeys)
	if !ok || pk.User != "git" {
		t.Fatalf("auth = %#v", m)
	}
	if _, err := AuthMethod("ssh://git@github.com/o/r.git", &config.GitAuth{SSHKeyFile: keyFile}); err != nil {
		t.Fatalf("ssh:// url: %v", err)
	}

	if m, err := AuthMethod("https://github.com/o/r.git", nil); err != nil || m != nil {
		t.Fatalf("nil auth = %v, %v", m, err)
	}
	if _, err := AuthMethod("https://github.com/o/r.git", &config.GitAuth{SSHKeyFile: keyFile}); err == nil {
		t.Fatal("ssh key with https url: want error")
	}
	if _, err := AuthMethod("http://github.com/o/r.git", &config.GitAuth{Token: "tok"}); err == nil || !strings.Contains(err.Error(), "token auth requires an https repo URL") {
		t.Fatalf("token with http url: err = %v", err)
	}
	if _, err := AuthMethod("git@github.com:o/r.git", &config.GitAuth{Token: "tok"}); err == nil {
		t.Fatal("token with scp url: want error")
	}
	if _, err := AuthMethod("ssh://git@github.com/o/r.git", &config.GitAuth{Token: "tok"}); err == nil {
		t.Fatal("token with ssh url: want error")
	}
	if _, err := AuthMethod("https://github.com/o/r.git", &config.GitAuth{Token: "t", SSHKeyFile: keyFile}); err == nil {
		t.Fatal("both token and key: want error")
	}
	if _, err := AuthMethod("git@github.com:o/r.git", &config.GitAuth{SSHKeyFile: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("missing key file: want error")
	}
}
