package manager

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"github.com/kenmoini/mcp-machine/internal/proxy"
	"github.com/kenmoini/mcp-machine/internal/store"
)

// The stdio pipeline: a container whose MCP session runs over its attached
// stdin/stdout.

// create replaces any previous container with a fresh one and starts it.
func (r *runner) create(ctx context.Context) {
	r.set(store.StateCreating, "")
	if err := r.discardContainer(ctx); err != nil {
		r.failAttempt(ctx, err)
		return
	}
	cfg := r.m.config()
	if cfg.Podman.Network != "" {
		if err := r.m.engine.EnsureNetwork(ctx, cfg.Podman.Network); err != nil {
			r.failAttempt(ctx, fmt.Errorf("ensure network %s: %w", cfg.Podman.Network, err))
			return
		}
	}
	spec := *r.resolved.Container
	if spec.Name == "" {
		spec.Name = store.ContainerName(r.name)
	}
	if spec.Network == "" {
		spec.Network = cfg.Podman.Network
	}
	spec.Image = r.image
	labels := maps.Clone(spec.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	maps.Copy(labels, store.Labels(r.name, r.hash, "stdio"))
	spec.Labels = labels
	spec.Stdin = true
	id, err := r.m.engine.CreateContainer(ctx, spec)
	if err != nil {
		r.failAttempt(ctx, fmt.Errorf("create container: %w", err))
		return
	}
	r.status.ContainerID = id
	r.attachAndStart(ctx)
}

// attachAndStart attaches to the container's stdio (before starting it, so
// no output is lost), starts it, and connects the facade to it.
func (r *runner) attachAndStart(ctx context.Context) {
	id := r.status.ContainerID
	rwc, err := r.m.engine.Attach(ctx, id)
	if err != nil {
		r.failAttempt(ctx, fmt.Errorf("attach: %w", err))
		return
	}
	if err := r.m.engine.StartContainer(ctx, id); err != nil {
		_ = rwc.Close()
		r.failAttempt(ctx, fmt.Errorf("start container: %w", err))
		return
	}
	// Cancelling the attempt must unblock a dial stuck writing to a backend
	// that never reads; context cancellation alone does not.
	context.AfterFunc(ctx, func() { _ = rwc.Close() })
	r.gen++
	gen := r.gen
	go r.waitExit(gen, id)
	r.set(store.StateRunning, "")

	r.set(store.StateConnecting, "")
	// The session lives on ctx, the attempt's long-lived context, not a
	// dial timeout: it must outlive the dial.
	sess, err := proxy.DialIO(ctx, rwc, r.facade.ClientOptions())
	if err != nil {
		_ = rwc.Close()
		if exitErr := r.exitedError(); exitErr != nil {
			r.failAttempt(ctx, exitErr)
			return
		}
		r.failAttempt(ctx, fmt.Errorf("connect: %w", err))
		r.awaitExit = ctx.Err() == nil
		return
	}
	r.sess, r.rwc = sess, rwc
	go func() {
		_ = sess.Wait()
		r.post(event{kind: evSessionLost, gen: gen})
	}()
	err = r.facade.Bind(ctx, sess)
	if ctx.Err() != nil {
		r.failAttempt(ctx, ctx.Err()) // interrupted: the preempting command decides
		return
	}
	if err != nil {
		if exitErr := r.exitedError(); exitErr != nil {
			err = exitErr
		} else {
			err = fmt.Errorf("bind: %w", err)
		}
		r.failAttempt(ctx, err)
		return
	}
	r.abandoned = false
	r.set(store.StateReady, "")
}

// waitExit reports the exit of container id, started as generation gen. It
// lives as long as the runner (not the attempt) so an exit that races a
// failed dial is still seen and can explain it.
func (r *runner) waitExit(gen uint64, id string) {
	code, err := r.m.engine.Wait(r.ctx, id)
	if err != nil {
		if r.ctx.Err() == nil {
			r.m.log.Debug("manager: wait failed", "server", r.name, "container", id, "err", err)
		}
		return
	}
	r.post(event{kind: evExit, gen: gen, code: code})
}

// exitedError returns the exit error if the container has exited, else nil.
func (r *runner) exitedError() error {
	if info, err := r.m.engine.InspectContainer(r.ctx, r.status.ContainerID); err == nil && info.State == "exited" {
		return r.exitError(info.ExitCode)
	}
	return nil
}

// exitError describes a container exit with its last log lines.
func (r *runner) exitError(code int) error {
	tail := "(none)"
	if lines, err := r.m.engine.Logs(r.ctx, r.status.ContainerID, exitLogLines); err == nil && len(lines) > 0 {
		texts := make([]string, len(lines))
		for i, l := range lines {
			texts[i] = l.Text
		}
		tail = strings.Join(texts, " | ")
	}
	return fmt.Errorf("container exited with code %d; last log lines: %s", code, tail)
}
