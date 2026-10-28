package build

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"

	"github.com/kenmoini/mcp-machine/internal/config"
)

var fullSHA = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// GoGitCloner is a Cloner backed by go-git. Branch and tag refs are fetched
// shallowly (depth 1); a full commit sha requires a full clone of the default
// branch followed by a checkout, so the commit must be reachable from it.
type GoGitCloner struct{}

// Clone implements Cloner. An unknown ref name is tried as a branch, then as
// a tag, then (if it is 40 hex characters) as a commit sha.
func (GoGitCloner) Clone(ctx context.Context, repo, ref, dir string, auth *config.GitAuth) (string, error) {
	am, err := AuthMethod(repo, auth)
	if err != nil {
		return "", err
	}
	attempts := []func() (string, error){
		func() (string, error) { return cloneRef(ctx, repo, dir, am, plumbing.ReferenceName("")) },
	}
	if ref != "" {
		attempts = []func() (string, error){
			func() (string, error) { return cloneRef(ctx, repo, dir, am, plumbing.NewBranchReferenceName(ref)) },
			func() (string, error) { return cloneRef(ctx, repo, dir, am, plumbing.NewTagReferenceName(ref)) },
		}
		if fullSHA.MatchString(ref) {
			attempts = append(attempts, func() (string, error) { return cloneSHA(ctx, repo, dir, am, ref) })
		}
	}
	var first error
	for _, attempt := range attempts {
		if err := os.RemoveAll(dir); err != nil {
			return "", err
		}
		sha, err := attempt()
		if err == nil {
			return sha, nil
		}
		if first == nil {
			first = err
		}
	}
	if ref == "" {
		return "", fmt.Errorf("clone %s: %w", repo, first)
	}
	return "", fmt.Errorf("clone %s at %q: %w", repo, ref, first)
}

// cloneRef shallow-clones one reference (the default branch when name is
// empty) and returns the commit it resolves to.
func cloneRef(ctx context.Context, repo, dir string, am transport.AuthMethod, name plumbing.ReferenceName) (string, error) {
	r, err := git.PlainCloneContext(ctx, dir, false, &git.CloneOptions{
		URL:           repo,
		Auth:          am,
		Depth:         1,
		SingleBranch:  true,
		ReferenceName: name,
	})
	if err != nil {
		return "", err
	}
	return headSHA(r)
}

// cloneSHA clones the default branch in full and checks out sha.
func cloneSHA(ctx context.Context, repo, dir string, am transport.AuthMethod, sha string) (string, error) {
	r, err := git.PlainCloneContext(ctx, dir, false, &git.CloneOptions{URL: repo, Auth: am})
	if err != nil {
		return "", err
	}
	wt, err := r.Worktree()
	if err != nil {
		return "", err
	}
	if err := wt.Checkout(&git.CheckoutOptions{Hash: plumbing.NewHash(sha), Force: true}); err != nil {
		return "", err
	}
	return headSHA(r)
}

func headSHA(r *git.Repository) (string, error) {
	h, err := r.Head()
	if err != nil {
		return "", err
	}
	return h.Hash().String(), nil
}

// AuthMethod builds the go-git auth method for repo from the configured
// credentials. It returns nil for no credentials. A token needs an https
// URL and an SSH key needs an SSH URL; a mismatch is an error.
//
// With an SSH key, the server's host key is checked against
// known_hosts_file when it is set; otherwise go-git's default applies when
// connecting: the files listed in SSH_KNOWN_HOSTS, else ~/.ssh/known_hosts
// and /etc/ssh/ssh_known_hosts.
func AuthMethod(repo string, a *config.GitAuth) (transport.AuthMethod, error) {
	if a != nil && a.KnownHostsFile != "" && a.SSHKeyFile == "" {
		return nil, errors.New("git auth: known_hosts_file requires ssh_key_file")
	}
	if a == nil || (a.Token == "" && a.SSHKeyFile == "") {
		return nil, nil
	}
	if a.Token != "" && a.SSHKeyFile != "" {
		return nil, errors.New("git auth: set only one of token and ssh_key_file")
	}
	if a.Token != "" {
		if !strings.HasPrefix(repo, "https://") {
			return nil, fmt.Errorf("git auth: token auth requires an https repo URL, got %q", repo)
		}
		return &githttp.BasicAuth{Username: "x-access-token", Password: a.Token}, nil
	}
	if !isSSHURL(repo) {
		return nil, fmt.Errorf("git auth: ssh_key_file requires an ssh repo URL, got %q", repo)
	}
	keys, err := gitssh.NewPublicKeysFromFile("git", a.SSHKeyFile, "")
	if err != nil {
		return nil, fmt.Errorf("git auth: load ssh key %s: %w", a.SSHKeyFile, err)
	}
	if a.KnownHostsFile != "" {
		db, err := gitssh.NewKnownHostsDb(a.KnownHostsFile)
		if err != nil {
			return nil, fmt.Errorf("git auth: load known_hosts_file %s: %w", a.KnownHostsFile, err)
		}
		keys.HostKeyCallback = db.HostKeyCallback()
		// Offer only the key types known for this host, so the server
		// does not pick one the file has no entry for.
		if hp, err := sshHostPort(repo); err == nil {
			keys.HostKeyAlgorithms = db.HostKeyAlgorithms(hp)
		}
	}
	return keys, nil
}

// sshHostPort returns the host:port an SSH repo URL connects to.
func sshHostPort(repo string) (string, error) {
	ep, err := transport.NewEndpoint(repo)
	if err != nil {
		return "", err
	}
	port := ep.Port
	if port == 0 {
		port = 22
	}
	return net.JoinHostPort(ep.Host, strconv.Itoa(port)), nil
}

// isSSHURL reports whether u is an ssh:// URL or scp-style (user@host:path).
func isSSHURL(u string) bool {
	if strings.HasPrefix(u, "ssh://") {
		return true
	}
	if strings.Contains(u, "://") {
		return false
	}
	at := strings.Index(u, "@")
	colon := strings.Index(u, ":")
	return at > 0 && colon > at
}
