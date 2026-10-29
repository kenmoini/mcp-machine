package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/runtime"
	"github.com/kenmoini/mcp-machine/internal/vault"
)

// Delivery of ${vault:...} secrets into a container.
//
// A container whose spec has SecretEnv gets a tmpfs at secretDir and, as
// its entrypoint, a small shell wrapper that waits for secretFile to
// appear, sources it, deletes it and execs the image's own entrypoint and
// command. After the container starts, the manager reads the secrets from
// Vault and streams NAME='value' lines into secretFile over an exec. The
// values are never in the create spec (which Podman persists on disk and
// shows in inspect), only in the container's memory.

const (
	deliveryDir  = "/run/mcp-machine"
	deliveryPath = deliveryDir + "/env"
	// wrapperPollSeconds is how long the wrapper sleeps between polls for
	// the file; it gives up after 600 of them.
	wrapperPollSeconds = "0.1"
)

// wrapperScript is the entrypoint wrapper. It is one sh -c argument, so it
// must not contain a single quote. $0 is the literal wrapperName and "$@"
// is the image's argv. sleep without sub-second support (not GNU, not
// busybox) falls back to whole seconds.
const wrapperScript = `f=` + deliveryPath + `
i=0
while [ ! -f "$f" ]; do
  i=$((i+1))
  if [ "$i" -ge 600 ]; then
    echo "mcp-machine: timed out waiting for secrets in $f" >&2
    exit 1
  fi
  sleep ` + wrapperPollSeconds + ` 2>/dev/null || sleep 1
done
set -a
. "$f"
set +a
rm -f "$f"
exec "$@"
`

// wrapperName is the wrapper's $0, visible in the process list.
const wrapperName = "mcp-machine"

// deliverCmd receives the env file on stdin and installs it atomically.
// umask 077 keeps the file private to the image's user, who runs both this
// exec and the wrapper.
var deliverCmd = []string{"sh", "-c",
	"umask 077 && cat > " + deliveryPath + ".tmp && mv " + deliveryPath + ".tmp " + deliveryPath}

// secretMount is the tmpfs the env file lives in: sticky and world-writable
// like /tmp (the tmpfs is root-owned, the image's user writes into it), with
// nothing executable, and small.
func secretMount() runtime.Mount {
	return runtime.Mount{Type: "tmpfs", Target: deliveryDir, Options: "nosuid,nodev,noexec,mode=1777,size=1m"}
}

// wrapEntrypoint makes the wrapper the container's entrypoint and the argv
// the container would otherwise run its command: the image's entrypoint
// followed by the spec's command, or by the image's command when the spec
// has none, as Podman composes them. A spec with its own entrypoint keeps
// it in place of the image's.
func wrapEntrypoint(spec *runtime.ContainerSpec, img runtime.ImageConfig) error {
	entrypoint := img.Entrypoint
	if len(spec.Entrypoint) > 0 {
		entrypoint = spec.Entrypoint
	}
	cmd := spec.Command
	if len(cmd) == 0 {
		cmd = img.Cmd
	}
	argv := slices.Concat(entrypoint, cmd)
	if len(argv) == 0 {
		return fmt.Errorf("image %s declares no entrypoint or command to wrap", spec.Image)
	}
	spec.Entrypoint = []string{"/bin/sh", "-c", wrapperScript, wrapperName}
	spec.Command = argv
	return nil
}

// envName is what a variable name sourced by sh may look like.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// envFile renders vals as NAME='value' lines, sorted by name, for sh to
// source. Values are single-quoted, so anything but a single quote is
// literal and a single quote becomes '\”. A name sh would not accept is an
// error: it could otherwise smuggle commands into the sourced file.
func envFile(vals map[string]string) ([]byte, error) {
	var b bytes.Buffer
	for _, k := range slices.Sorted(keys(vals)) {
		if !envName.MatchString(k) {
			return nil, fmt.Errorf("secrets: %q is not a valid environment variable name", k)
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(shellQuote(vals[k]))
		b.WriteByte('\n')
	}
	return b.Bytes(), nil
}

// shellQuote single-quotes s for sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func keys(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// errNoVault is the failure of a server that references Vault secrets when
// no Vault is configured.
var errNoVault = errors.New("vault is not configured; the server references ${vault:...}")

// installSecrets prepares spec for secret delivery when it has SecretEnv:
// the wrapper entrypoint (which needs the image's configuration) and the
// tmpfs. It returns the resolved values to deliver after the container
// starts, or nil when the spec has no secrets. The values are read from
// Vault here, before anything is created, so a Vault problem creates
// nothing. spec.SecretEnv is cleared.
func (r *runner) installSecrets(ctx context.Context, spec *runtime.ContainerSpec) (map[string]string, error) {
	if len(spec.SecretEnv) == 0 {
		return nil, nil
	}
	vals, err := r.prepareSecrets(ctx)
	if err != nil {
		return nil, err
	}
	img, err := r.m.engine.ImageConfig(ctx, spec.Image)
	if err != nil {
		return nil, fmt.Errorf("inspect image %s: %w", spec.Image, err)
	}
	if err := wrapEntrypoint(spec, img); err != nil {
		return nil, err
	}
	spec.Mounts = append(slices.Clone(spec.Mounts), secretMount())
	spec.SecretEnv = nil
	return vals, nil
}

// prepareSecrets reads the server's SecretEnv values from Vault. It returns
// nil without Vault access when the server has none to read.
func (r *runner) prepareSecrets(ctx context.Context) (map[string]string, error) {
	if r.resolved == nil || r.resolved.Container == nil || len(r.resolved.Container.SecretEnv) == 0 {
		return nil, nil
	}
	if r.m.vault == nil {
		return nil, errNoVault
	}
	vals, err := vault.ResolveMap(ctx, r.m.vault, r.resolved.Container.SecretEnv)
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	return vals, nil
}

// deliverSecrets streams vals into the running container id as the env
// file the wrapper waits for. The rendered bytes and vals are wiped
// afterwards. Nothing about the values is logged.
func (r *runner) deliverSecrets(ctx context.Context, id string, vals map[string]string) error {
	file, err := envFile(vals)
	defer func() {
		clear(file)
		for k := range vals {
			vals[k] = ""
		}
	}()
	if err != nil {
		return err
	}
	code, err := r.m.engine.Exec(ctx, id, deliverCmd, bytes.NewReader(file))
	if err != nil {
		return fmt.Errorf("deliver secrets: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("deliver secrets: exit %d", code)
	}
	r.m.log.Debug("manager: secrets delivered", "server", r.name, "container", id, "vars", len(vals))
	return nil
}

// startedWithSecrets delivers vals into the container just started, when
// there are any. A failure is explained by the container's exit when it has
// already exited (an image without /bin/sh, say).
func (r *runner) startedWithSecrets(ctx context.Context, id string, vals map[string]string) error {
	if len(vals) == 0 {
		return nil
	}
	if err := r.deliverSecrets(ctx, id, vals); err != nil {
		if ctx.Err() == nil {
			if exitErr := r.exitedError(); exitErr != nil {
				return exitErr
			}
		}
		return err
	}
	return nil
}

// resolveStrings returns a copy of m with its ${vault:...} references
// resolved, or m itself when it has none. Without Vault, a reference is
// errNoVault.
func (r *runner) resolveStrings(ctx context.Context, m map[string]string) (map[string]string, error) {
	refs := false
	for _, v := range m {
		if config.HasVaultRef(v) {
			refs = true
			break
		}
	}
	if !refs {
		return m, nil
	}
	if r.m.vault == nil {
		return nil, errNoVault
	}
	return vault.ResolveMap(ctx, r.m.vault, m)
}

// resolveAuth returns auth with its ${vault:...} references resolved (a copy
// when there are any).
func (r *runner) resolveAuth(ctx context.Context, auth *runtime.RegistryAuth) (*runtime.RegistryAuth, error) {
	if auth == nil || (!config.HasVaultRef(auth.Username) && !config.HasVaultRef(auth.Password)) {
		return auth, nil
	}
	m, err := r.resolveStrings(ctx, map[string]string{"username": auth.Username, "password": auth.Password})
	if err != nil {
		return nil, err
	}
	return &runtime.RegistryAuth{Username: m["username"], Password: m["password"]}, nil
}

// The integration tests drive a real engine through the same wrapper, mount
// and delivery command as the manager.

// WrapEntrypoint is wrapEntrypoint.
func WrapEntrypoint(spec *runtime.ContainerSpec, img runtime.ImageConfig) error {
	return wrapEntrypoint(spec, img)
}

// SecretMount is the tmpfs the secrets are delivered into.
func SecretMount() runtime.Mount { return secretMount() }

// DeliverCommand is the command that receives the env file on stdin.
func DeliverCommand() []string { return slices.Clone(deliverCmd) }

// EnvFile renders vals as the file DeliverCommand receives.
func EnvFile(vals map[string]string) ([]byte, error) { return envFile(vals) }

// DeliveryPath is where the wrapper expects the env file.
const DeliveryPath = deliveryPath
