// Package build resolves "build" server specs and implements the pipeline
// that clones a git repository and builds an image from it.
package build

import (
	"context"

	"github.com/kenmoini/mcp-machine/internal/config"
)

// Cloner fetches a git repository into a directory.
type Cloner interface {
	// Clone checks out repo at ref (a branch, tag or full commit sha; empty
	// means the default branch) into dir and returns the resolved commit sha.
	Clone(ctx context.Context, repo, ref, dir string, auth *config.GitAuth) (sha string, err error)
}
