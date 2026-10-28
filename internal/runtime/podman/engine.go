// Package podman implements runtime.Engine over the Podman libpod REST API
// using the Go bindings. It is built with the "remote" tag, so it only ever
// talks to a Podman socket and never links the local container stack.
package podman

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	buildahDefine "go.podman.io/buildah/define"
	nettypes "go.podman.io/common/libnetwork/types"
	imageTypes "go.podman.io/image/v5/types"
	"go.podman.io/podman/v6/libpod/define"
	"go.podman.io/podman/v6/pkg/bindings"
	"go.podman.io/podman/v6/pkg/bindings/containers"
	"go.podman.io/podman/v6/pkg/bindings/images"
	"go.podman.io/podman/v6/pkg/bindings/network"
	"go.podman.io/podman/v6/pkg/bindings/system"
	"go.podman.io/podman/v6/pkg/domain/entities/types"
	"go.podman.io/podman/v6/pkg/errorhandling"

	"github.com/kenmoini/mcp-machine/internal/runtime"
)

// Engine is a runtime.Engine backed by a Podman socket.
type Engine struct {
	connCtx   context.Context // carries the bindings client; derive every call ctx from it
	socketURL string          // used to dial private connections for attachments
	log       *slog.Logger
}

var _ runtime.Engine = (*Engine)(nil)

// New connects to the Podman API at socketURL (for example
// "unix:///run/podman/podman.sock"). The stored connection context is
// detached from ctx's cancellation so a short-lived startup ctx cannot kill
// later calls. The Engine has no teardown: the connection lives for the
// life of the process.
func New(ctx context.Context, socketURL string) (*Engine, error) {
	connCtx, err := bindings.NewConnection(context.WithoutCancel(ctx), socketURL)
	if err != nil {
		return nil, fmt.Errorf("podman: connect %s: %w", socketURL, err)
	}
	return &Engine{connCtx: connCtx, socketURL: socketURL, log: slog.Default()}, nil
}

// ctx derives a call context that carries the bindings client but is
// cancelled when the caller's ctx is. The returned func must be called when
// the operation finishes.
func (e *Engine) ctx(caller context.Context) (context.Context, func()) {
	c, cancel := context.WithCancel(e.connCtx)
	stop := context.AfterFunc(caller, cancel)
	return c, func() {
		stop()
		cancel()
	}
}

// mapErr converts Podman 404 responses to runtime.ErrNotFound.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var em *errorhandling.ErrorModel
	if errors.As(err, &em) && em.ResponseCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s", runtime.ErrNotFound, em.Message)
	}
	return err
}

func (e *Engine) Ping(ctx context.Context) error {
	c, done := e.ctx(ctx)
	defer done()
	_, err := system.Version(c, nil)
	return err
}

func (e *Engine) EnsureNetwork(ctx context.Context, name string) error {
	c, done := e.ctx(ctx)
	defer done()
	ok, err := network.Exists(c, name, nil)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	_, err = network.Create(c, &nettypes.Network{Name: name, DNSEnabled: true})
	if isAlreadyExists(err) {
		return nil // lost a race with a concurrent caller
	}
	return mapErr(err)
}

// isAlreadyExists reports whether err is a Podman 409 / "already exists".
func isAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	var em *errorhandling.ErrorModel
	if errors.As(err, &em) && em.ResponseCode == http.StatusConflict {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "already exists")
}

func (e *Engine) ConnectNetwork(ctx context.Context, containerID, networkName string) error {
	c, done := e.ctx(ctx)
	defer done()
	return mapErr(network.Connect(c, networkName, containerID, &nettypes.PerNetworkOptions{}))
}

func (e *Engine) ImageExists(ctx context.Context, ref string) (bool, error) {
	c, done := e.ctx(ctx)
	defer done()
	return images.Exists(c, ref, nil)
}

func (e *Engine) PullImage(ctx context.Context, ref string, auth *runtime.RegistryAuth, progress io.Writer) error {
	c, done := e.ctx(ctx)
	defer done()
	opts := &images.PullOptions{Quiet: ptr(progress == nil)}
	if progress != nil {
		opts.ProgressWriter = &progress
	}
	if auth != nil {
		opts.Username = ptr(auth.Username)
		opts.Password = ptr(auth.Password)
	}
	_, err := images.Pull(c, ref, opts)
	return mapErr(err)
}

func (e *Engine) BuildImage(ctx context.Context, req runtime.BuildRequest) (string, error) {
	c, done := e.ctx(ctx)
	defer done()

	labels := make([]string, 0, len(req.Labels))
	for _, k := range slices.Sorted(maps.Keys(req.Labels)) {
		labels = append(labels, k+"="+req.Labels[k])
	}
	bo := buildahDefine.BuildOptions{
		ContextDirectory: req.ContextDir,
		Output:           req.Tag,
		Args:             req.Args,
		Labels:           labels,
		Layers:           true,
	}
	if req.Auth != nil {
		bo.SystemContext = &imageTypes.SystemContext{
			DockerAuthConfig: &imageTypes.DockerAuthConfig{Username: req.Auth.Username, Password: req.Auth.Password},
		}
	}
	files := []string{containerfilePath(req.ContextDir, req.Containerfile)}
	report, err := images.Build(c, files, types.BuildOptions{BuildOptions: bo, ContainerFiles: files})
	if err != nil {
		return "", mapErr(err)
	}
	return report.ID, nil
}

// containerfilePath returns the Containerfile to build with. A relative path
// is relative to the build context; images.Build would otherwise resolve it
// against the process's working directory.
func containerfilePath(contextDir, containerfile string) string {
	if containerfile == "" {
		containerfile = "Containerfile"
	}
	if filepath.IsAbs(containerfile) {
		return containerfile
	}
	return filepath.Join(contextDir, containerfile)
}

func (e *Engine) CreateContainer(ctx context.Context, spec runtime.ContainerSpec) (string, error) {
	c, done := e.ctx(ctx)
	defer done()
	resp, err := containers.CreateWithSpec(c, toSpecGen(spec), nil)
	if err != nil {
		return "", mapErr(err)
	}
	return resp.ID, nil
}

func (e *Engine) StartContainer(ctx context.Context, id string) error {
	c, done := e.ctx(ctx)
	defer done()
	return mapErr(containers.Start(c, id, nil))
}

func (e *Engine) StopContainer(ctx context.Context, id string, timeout time.Duration) error {
	c, done := e.ctx(ctx)
	defer done()
	return mapErr(containers.Stop(c, id, &containers.StopOptions{Timeout: ptr(stopSeconds(timeout))}))
}

// stopSeconds converts a stop timeout to whole seconds, rounding up and
// clamping negative values to 0.
func stopSeconds(d time.Duration) uint {
	if d <= 0 {
		return 0
	}
	return uint((d + time.Second - 1) / time.Second)
}

func (e *Engine) RemoveContainer(ctx context.Context, id string, force bool) error {
	c, done := e.ctx(ctx)
	defer done()
	_, err := containers.Remove(c, id, &containers.RemoveOptions{Force: ptr(force)})
	return mapErr(err)
}

func (e *Engine) InspectContainer(ctx context.Context, id string) (runtime.ContainerInfo, error) {
	c, done := e.ctx(ctx)
	defer done()
	d, err := containers.Inspect(c, id, nil)
	if err != nil {
		return runtime.ContainerInfo{}, mapErr(err)
	}
	info := runtime.ContainerInfo{
		ID:    d.ID,
		Name:  strings.TrimPrefix(d.Name, "/"),
		Image: d.ImageName,
	}
	if d.Config != nil {
		info.Labels = d.Config.Labels
	}
	if d.State != nil {
		info.State = d.State.Status
		info.ExitCode = int(d.State.ExitCode)
	}
	if d.NetworkSettings != nil {
		info.Ports = inspectPorts(d.NetworkSettings.Ports)
		info.Networks = sortedNetworkNames(d.NetworkSettings.Networks)
	}
	return info, nil
}

// sortedNetworkNames returns the sorted keys of an inspect network map.
func sortedNetworkNames(m map[string]*define.InspectAdditionalNetwork) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// inspectPorts converts inspect-style port bindings ("3000/tcp" ->
// [{HostIP, HostPort}]) into runtime port mappings. The result is sorted by
// container port, protocol, host IP, then host port, so it is deterministic
// despite the map input.
func inspectPorts(m map[string][]define.InspectHostPort) []runtime.PortMapping {
	var out []runtime.PortMapping
	for key, binds := range m {
		portStr, proto, _ := strings.Cut(key, "/")
		cport, err := strconv.Atoi(portStr)
		if err != nil {
			continue
		}
		for _, b := range binds {
			hport, err := strconv.Atoi(b.HostPort)
			if err != nil {
				continue
			}
			out = append(out, runtime.PortMapping{HostIP: b.HostIP, HostPort: hport, ContainerPort: cport, Protocol: proto})
		}
	}
	slices.SortFunc(out, func(a, b runtime.PortMapping) int {
		return cmp.Or(
			cmp.Compare(a.ContainerPort, b.ContainerPort),
			cmp.Compare(a.Protocol, b.Protocol),
			cmp.Compare(a.HostIP, b.HostIP),
			cmp.Compare(a.HostPort, b.HostPort),
		)
	})
	return out
}

func (e *Engine) ListContainers(ctx context.Context, labels map[string]string) ([]runtime.ContainerInfo, error) {
	c, done := e.ctx(ctx)
	defer done()
	list, err := containers.List(c, &containers.ListOptions{All: ptr(true), Filters: labelFilter(labels)})
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]runtime.ContainerInfo, 0, len(list))
	for _, lc := range list {
		info := runtime.ContainerInfo{
			ID:       lc.ID,
			Image:    lc.Image,
			State:    lc.State,
			Labels:   lc.Labels,
			ExitCode: int(lc.ExitCode),
		}
		if len(lc.Networks) > 0 {
			info.Networks = append([]string(nil), lc.Networks...)
			slices.Sort(info.Networks)
		}
		if len(lc.Names) > 0 {
			info.Name = lc.Names[0]
		}
		for _, p := range lc.Ports {
			info.Ports = append(info.Ports, runtime.PortMapping{
				HostIP: p.HostIP, HostPort: int(p.HostPort), ContainerPort: int(p.ContainerPort), Protocol: p.Protocol,
			})
		}
		out = append(out, info)
	}
	return out, nil
}

func (e *Engine) Logs(ctx context.Context, id string, tail int) ([]runtime.LogLine, error) {
	c, done := e.ctx(ctx)
	defer done()

	tailArg := "all"
	if tail > 0 {
		tailArg = strconv.Itoa(tail)
	}
	stdout := make(chan string)
	stderr := make(chan string)
	collected := make(chan []runtime.LogLine, 1)
	go func() { collected <- collectLogs(stdout, stderr) }()

	err := containers.Logs(c, id, &containers.LogOptions{
		Stdout: ptr(true), Stderr: ptr(true), Tail: ptr(tailArg),
	}, stdout, stderr)
	// Logs sends synchronously and has returned, so closing is race-free.
	close(stdout)
	close(stderr)
	lines := <-collected
	if err != nil {
		return nil, mapErr(err)
	}
	return lines, nil
}

// collectLogs drains both channels until they are closed, splitting frames
// into lines. Partial trailing lines are carried over per stream.
func collectLogs(stdout, stderr <-chan string) []runtime.LogLine {
	var lines []runtime.LogLine
	pending := map[string]string{}
	add := func(stream, chunk string) {
		buf := pending[stream] + chunk
		for {
			i := strings.IndexByte(buf, '\n')
			if i < 0 {
				break
			}
			lines = append(lines, runtime.LogLine{Stream: stream, Text: strings.TrimSuffix(buf[:i], "\r")})
			buf = buf[i+1:]
		}
		pending[stream] = buf
	}
	for stdout != nil || stderr != nil {
		select {
		case s, ok := <-stdout:
			if !ok {
				stdout = nil
				continue
			}
			add("stdout", s)
		case s, ok := <-stderr:
			if !ok {
				stderr = nil
				continue
			}
			add("stderr", s)
		}
	}
	for _, stream := range []string{"stdout", "stderr"} {
		if rest := pending[stream]; rest != "" {
			lines = append(lines, runtime.LogLine{Stream: stream, Text: rest})
		}
	}
	return lines
}

func (e *Engine) Wait(ctx context.Context, id string) (int, error) {
	c, done := e.ctx(ctx)
	defer done()
	code, err := containers.Wait(c, id, nil)
	if err != nil {
		return 0, mapErr(err)
	}
	return int(code), nil
}
