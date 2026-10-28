package manager

import (
	"context"
	"fmt"
	"strings"

	"github.com/kenmoini/mcp-machine/internal/proxy"
	"github.com/kenmoini/mcp-machine/internal/store"
)

// The stdio pipeline: a container whose MCP session runs over its attached
// stdin/stdout. The container exit helpers at the end are shared with the
// http pipeline (runner_http.go).

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
	go r.waitExit(gen, id, nil)
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
	r.rwc = rwc
	r.bindSession(ctx, sess, gen)
}

// waitExit reports the exit of container id, started as generation gen. It
// lives as long as the runner (not the attempt) so an exit that races a
// failed dial is still seen and can explain it. exited, if not nil, is
// closed when the container exits, before the event is posted.
func (r *runner) waitExit(gen uint64, id string, exited chan<- struct{}) {
	code, err := r.m.engine.Wait(r.ctx, id)
	if err != nil {
		if r.ctx.Err() == nil {
			r.m.log.Debug("manager: wait failed", "server", r.name, "container", id, "err", err)
		}
		return
	}
	if exited != nil {
		close(exited)
	}
	r.post(event{kind: evExit, gen: gen, code: code})
}

// exitedError returns the exit error if the container has exited, else nil
// (always nil for a server without a container).
func (r *runner) exitedError() error {
	if r.status.ContainerID == "" {
		return nil
	}
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
