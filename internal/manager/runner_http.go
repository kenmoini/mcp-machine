package manager

import (
	"context"
	"fmt"

	"github.com/kenmoini/mcp-machine/internal/proxy"
	"github.com/kenmoini/mcp-machine/internal/runtime"
	"github.com/kenmoini/mcp-machine/internal/sources"
	"github.com/kenmoini/mcp-machine/internal/store"
)

// The http pipeline (a container serving MCP over HTTP on a port) and the
// remote pipeline (a server MCP Machine only connects to).

// httpPorts returns the container's port mappings adjusted so MCP Machine
// can reach port, the server's MCP port. A mapping of port without a fixed
// host port exists only for that purpose and is replaced: on the host by one
// bound to loopback with an ephemeral host port, in a container by nothing,
// since the server is then reached over the shared network. Other mappings,
// including a fixed host port for port, are kept as configured.
func httpPorts(ports []runtime.PortMapping, port int, inContainer bool) []runtime.PortMapping {
	var out []runtime.PortMapping
	fixed := false
	for _, p := range ports {
		if p.ContainerPort == port && p.HostPort == 0 {
			continue
		}
		if p.ContainerPort == port {
			fixed = true
		}
		out = append(out, p)
	}
	if !inContainer && !fixed {
		out = append(out, runtime.PortMapping{HostIP: "127.0.0.1", HostPort: 0, ContainerPort: port, Protocol: "tcp"})
	}
	return out
}

// resumeHTTP restarts a stopped (or adopted) http-mode container, or, if it
// is already running, only connects to it.
func (r *runner) resumeHTTP(ctx context.Context, running bool) {
	if r.resolved == nil || r.resolved.HTTP == nil {
		// An adopted container: the endpoint comes from the spec.
		r.set(store.StateResolving, "")
		res, err := r.m.resolver.Resolve(ctx, r.m.config(), r.spec)
		if err != nil {
			r.failAttempt(ctx, fmt.Errorf("resolve: %w", err))
			return
		}
		if res.Kind != sources.KindContainer || res.HTTP == nil {
			r.failAttempt(ctx, fmt.Errorf("manager: server %q no longer resolves to an http container", r.name))
			return
		}
		r.resolved = &res
	}
	var secrets map[string]string
	if !running {
		r.set(store.StateCreating, "")
		var err error
		if secrets, err = r.prepareSecrets(ctx); err != nil {
			r.failAttempt(ctx, err)
			return
		}
	}
	r.startHTTP(ctx, running, secrets)
}

// startHTTP starts the container unless it is already running, delivering
// its secrets (secrets, when not empty) right after the start, waits for
// its MCP endpoint to answer, and connects the facade to it. A container
// found running (adopted) has its environment already.
func (r *runner) startHTTP(ctx context.Context, running bool, secrets map[string]string) {
	id := r.status.ContainerID
	if !running {
		if err := r.m.engine.StartContainer(ctx, id); err != nil {
			r.failAttempt(ctx, fmt.Errorf("start container: %w", err))
			return
		}
		if err := r.startedWithSecrets(ctx, id, secrets); err != nil {
			r.failAttempt(ctx, err)
			return
		}
	}
	r.gen++
	gen := r.gen
	exited := make(chan struct{})
	go r.waitExit(gen, id, exited)
	r.set(store.StateRunning, "")

	r.set(store.StateConnecting, "")
	ep := *r.resolved.HTTP
	info, err := r.m.engine.InspectContainer(ctx, id)
	if err != nil {
		r.failAttempt(ctx, fmt.Errorf("inspect container: %w", err))
		return
	}
	url, err := r.m.opts.EndpointFunc(info, ep)
	if err != nil {
		r.failAttempt(ctx, fmt.Errorf("endpoint: %w", err))
		return
	}

	// A container that exits while being probed ends the probe at once: its
	// exit, not the probe timeout, is the error.
	pctx, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-exited:
			cancel()
		case <-pctx.Done():
		}
	}()
	err = probe(pctx, url, r.m.opts.ProbeTimeout, probeInterval)
	cancel()
	if err != nil {
		r.connectFailed(ctx, err)
		return
	}

	// The session lives on ctx, the attempt's long-lived context (cancelled
	// only by teardown or an interrupt): an SSE session's stream is tied to
	// the context it was dialled with.
	sess, err := proxy.DialHTTP(ctx, url, ep.Transport, nil, r.facade.ClientOptions())
	if err != nil {
		r.connectFailed(ctx, fmt.Errorf("connect: %w", err))
		return
	}
	r.bindSession(ctx, sess, gen)
}

// connectFailed fails an attempt whose probe or dial failed, preferring the
// container's exit as the explanation; if the exit is not visible yet, the
// exit event that follows replaces err (awaitExit).
func (r *runner) connectFailed(ctx context.Context, err error) {
	if ctx.Err() == nil {
		if exitErr := r.exitedError(); exitErr != nil {
			r.fail(exitErr)
			return
		}
	}
	r.failAttempt(ctx, err)
	r.awaitExit = ctx.Err() == nil
}

// connectRemote dials a remote server and binds the facade to it. It makes
// no engine calls. Headers holding ${vault:...} references are resolved for
// this attempt only, so a restart picks up a rotated secret.
func (r *runner) connectRemote(ctx context.Context) {
	rem := r.resolved.Remote
	r.gen++ // tells this session's loss from an earlier one's
	gen := r.gen
	r.set(store.StateConnecting, "")
	headers, err := r.resolveStrings(ctx, rem.Headers)
	if err != nil {
		r.failAttempt(ctx, fmt.Errorf("headers: %w", err))
		return
	}
	sess, err := proxy.DialHTTP(ctx, rem.URL, rem.Transport, headers, r.facade.ClientOptions())
	if err != nil {
		r.failAttempt(ctx, fmt.Errorf("connect: %w", err))
		return
	}
	r.bindSession(ctx, sess, gen)
}
