package build

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kenmoini/mcp-machine/internal/runtime"
	"github.com/kenmoini/mcp-machine/internal/sources"
	"github.com/kenmoini/mcp-machine/internal/store"
)

// LabelCommit is stamped on built images with the commit they were built from.
const LabelCommit = "io.mcp-machine/commit"

// Builder clones a plan's repository and builds an image from it.
type Builder struct {
	Workspace string
	Engine    runtime.Engine
	Cloner    Cloner
}

// Build clones the plan's repo into <Workspace>/<name>/src (re-cloned every
// time, so a moving ref such as a branch is re-resolved) and builds the image,
// returning its tag "localhost/mcp-machine/<name>:<short-sha>". The build is
// skipped when that tag already exists, unless force is set.
func (b *Builder) Build(ctx context.Context, plan sources.BuildPlan, force bool) (string, error) {
	dir := filepath.Join(b.Workspace, plan.Name, "src")
	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("clean %s: %w", dir, err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", fmt.Errorf("create workspace for %s: %w", plan.Name, err)
	}
	sha, err := b.Cloner.Clone(ctx, plan.Repo, plan.Ref, dir, plan.Auth)
	if err != nil {
		return "", err
	}
	if len(sha) < 7 {
		return "", fmt.Errorf("clone %s returned invalid commit %q", plan.Repo, sha)
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
	_, err = b.Engine.BuildImage(ctx, runtime.BuildRequest{
		ContextDir:    filepath.Join(dir, plan.Subdir),
		Containerfile: plan.Containerfile,
		Tag:           tag,
		Args:          plan.Args,
		Labels:        map[string]string{store.LabelServer: plan.Name, LabelCommit: sha},
		Auth:          plan.RegistryAuth,
	})
	if err != nil {
		return "", fmt.Errorf("build image %s: %w", tag, err)
	}
	return tag, nil
}
