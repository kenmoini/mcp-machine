// Package depcheck exists only to pin direct dependencies in go.mod before the
// real packages import them. It is deleted once the code base uses them.
package depcheck

import (
	_ "github.com/fsnotify/fsnotify"
	_ "github.com/go-git/go-git/v5"
	_ "github.com/go-git/go-git/v5/plumbing/transport/http"
	_ "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	_ "github.com/modelcontextprotocol/go-sdk/mcp"
	_ "go.podman.io/buildah/define"
	_ "go.podman.io/common/libnetwork/types"
	_ "go.podman.io/image/v5/types"
	_ "go.podman.io/podman/v6/pkg/bindings"
	_ "go.podman.io/podman/v6/pkg/bindings/containers"
	_ "go.podman.io/podman/v6/pkg/bindings/images"
	_ "go.podman.io/podman/v6/pkg/bindings/network"
	_ "go.podman.io/podman/v6/pkg/bindings/system"
	_ "go.podman.io/podman/v6/pkg/domain/entities/types"
	_ "go.podman.io/podman/v6/pkg/specgen"
	_ "gopkg.in/yaml.v3"
	_ "mvdan.cc/sh/v3/shell"
)
