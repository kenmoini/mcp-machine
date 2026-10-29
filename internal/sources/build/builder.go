package build

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/runtime"
	"github.com/kenmoini/mcp-machine/internal/sources"
	"github.com/kenmoini/mcp-machine/internal/store"
	"github.com/kenmoini/mcp-machine/internal/vault"
)

// LabelCommit is stamped on built images with the commit they were built from.
const LabelCommit = "io.mcp-machine/commit"

// Builder clones a plan's repository and builds an image from it.
type Builder struct {
	Workspace string
	Engine    runtime.Engine
	Cloner    Cloner
	// Registries supplies credentials for the Containerfile's base image.
	Registries []config.Registry
	// Secrets resolves ${vault:...} references in the git token and the
	// registry credentials, at the moment they are used. Nil means no Vault
	// is configured: a reference is then an error.
	Secrets vault.Reader
}

// errNoVault is the error for a ${vault:...} reference without a Vault.
var errNoVault = errors.New("vault is not configured; the credentials reference ${vault:...}")

// Build clones the plan's repo into <Workspace>/<name>/src (re-cloned every
// time, so a moving ref such as a branch is re-resolved) and builds the image,
// returning its tag "localhost/mcp-machine/<name>:<short-sha>". The build is
// skipped when that tag already exists, unless force is set.
func (b *Builder) Build(ctx context.Context, plan sources.BuildPlan, force bool) (string, error) {
	dir, err := b.srcDir(plan)
	if err != nil {
		return "", err
	}
	subdir, err := cleanSubdir(plan.Subdir)
	if err != nil {
		return "", err
	}
	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("clean %s: %w", dir, err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		return "", fmt.Errorf("create workspace for %s: %w", plan.Name, err)
	}
	gitAuth, err := b.resolveGitAuth(ctx, plan.Auth)
	if err != nil {
		return "", fmt.Errorf("git credentials: %w", err)
	}
	sha, err := b.Cloner.Clone(ctx, plan.Repo, plan.Ref, dir, gitAuth)
	if err != nil {
		return "", err
	}
	if len(sha) < 7 {
		return "", fmt.Errorf("clone %s returned invalid commit %q", plan.Repo, sha)
	}
	ctxDir, err := contained(dir, subdir)
	if err != nil {
		return "", fmt.Errorf("build subdir %q: %w", plan.Subdir, err)
	}
	tag := "localhost/mcp-machine/" + plan.Name + ":" + sha[:7]
	if !force {
		ok, err := b.Engine.ImageExists(ctx, tag)
		if err != nil {
			return "", fmt.Errorf("check image %s: %w", tag, err)
		}
		if ok {
			return tag, nil
		}
	}
	auth := plan.RegistryAuth
	if auth == nil && len(b.Registries) > 0 {
		cf, err := contained(dir, filepath.Join(subdir, plan.Containerfile))
		if err != nil {
			return "", fmt.Errorf("containerfile %q: %w", plan.Containerfile, err)
		}
		ref, err := baseImage(cf, plan.Args)
		if err != nil {
			return "", err
		}
		if ref != "" {
			auth = sources.RegistryAuthFor(b.Registries, ref)
		}
	}
	if auth, err = b.resolveRegistryAuth(ctx, auth); err != nil {
		return "", fmt.Errorf("registry credentials: %w", err)
	}
	_, err = b.Engine.BuildImage(ctx, runtime.BuildRequest{
		ContextDir:    ctxDir,
		Containerfile: plan.Containerfile,
		Tag:           tag,
		Args:          plan.Args,
		Labels:        map[string]string{store.LabelServer: plan.Name, LabelCommit: sha},
		Auth:          auth,
	})
	if err != nil {
		return "", fmt.Errorf("build image %s: %w", tag, err)
	}
	return tag, nil
}

// resolveGitAuth returns auth with a ${vault:...} token resolved (a copy),
// or auth itself when there is nothing to resolve.
func (b *Builder) resolveGitAuth(ctx context.Context, auth *config.GitAuth) (*config.GitAuth, error) {
	if auth == nil || !config.HasVaultRef(auth.Token) {
		return auth, nil
	}
	if b.Secrets == nil {
		return nil, errNoVault
	}
	token, err := vault.Resolve(ctx, b.Secrets, auth.Token)
	if err != nil {
		return nil, err
	}
	out := *auth
	out.Token = token
	return &out, nil
}

// resolveRegistryAuth is resolveGitAuth for registry credentials.
func (b *Builder) resolveRegistryAuth(ctx context.Context, auth *runtime.RegistryAuth) (*runtime.RegistryAuth, error) {
	if auth == nil || (!config.HasVaultRef(auth.Username) && !config.HasVaultRef(auth.Password)) {
		return auth, nil
	}
	if b.Secrets == nil {
		return nil, errNoVault
	}
	m, err := vault.ResolveMap(ctx, b.Secrets, map[string]string{"u": auth.Username, "p": auth.Password})
	if err != nil {
		return nil, err
	}
	return &runtime.RegistryAuth{Username: m["u"], Password: m["p"]}, nil
}

// srcDir validates the workspace and plan name and returns the clone
// directory, guaranteed to lie strictly under the workspace.
func (b *Builder) srcDir(plan sources.BuildPlan) (string, error) {
	if b.Workspace == "" || !filepath.IsAbs(b.Workspace) {
		return "", fmt.Errorf("workspace %q must be an absolute path", b.Workspace)
	}
	if !config.NameRE.MatchString(plan.Name) {
		return "", fmt.Errorf("invalid server name %q", plan.Name)
	}
	ws := filepath.Clean(b.Workspace)
	dir := filepath.Join(ws, plan.Name, "src")
	rel, err := filepath.Rel(ws, dir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("source directory %q is outside workspace %q", dir, ws)
	}
	return dir, nil
}

// cleanSubdir rejects absolute subdirectories and ones that climb out of the
// clone, returning the cleaned relative path ("." for none).
func cleanSubdir(sub string) (string, error) {
	if filepath.IsAbs(sub) {
		return "", fmt.Errorf("build subdir %q must be relative", sub)
	}
	c := filepath.Clean(sub)
	if c == ".." || strings.HasPrefix(c, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("build subdir %q escapes the repository", sub)
	}
	return c, nil
}

// contained joins root and rel, resolves symlinks, and returns the result only
// if it stays under root (also symlink-resolved).
func contained(root, rel string) (string, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	p, err := filepath.EvalSymlinks(filepath.Join(root, rel))
	if err != nil {
		return "", err
	}
	r, err := filepath.Rel(realRoot, p)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", errors.New("resolves outside the repository")
	}
	return filepath.Join(root, rel), nil
}

// baseImage returns the first real image named by a FROM instruction in the
// Containerfile at path: FROM is case-insensitive, flags such as --platform
// are skipped, "scratch" and references to earlier stage aliases are ignored.
// ARG references are expanded from args; unresolved ones are skipped. It
// returns "" if there is none.
func baseImage(path string, args map[string]string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path is the Containerfile inside the workspace this builder just cloned
	if err != nil {
		return "", fmt.Errorf("read containerfile: %w", err)
	}
	defer func() { _ = f.Close() }()
	stages := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || !strings.EqualFold(fields[0], "FROM") {
			continue
		}
		fields = fields[1:]
		for len(fields) > 0 && strings.HasPrefix(fields[0], "--") {
			fields = fields[1:]
		}
		if len(fields) == 0 {
			continue
		}
		ref := os.Expand(fields[0], func(k string) string { return args[k] })
		skip := ref == "" || strings.Contains(ref, "$") || strings.EqualFold(ref, "scratch") || stages[strings.ToLower(ref)]
		if len(fields) >= 3 && strings.EqualFold(fields[1], "AS") {
			stages[strings.ToLower(fields[2])] = true
		}
		if !skip {
			return ref, nil
		}
	}
	return "", sc.Err()
}
