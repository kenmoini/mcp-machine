package podman

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"

	dockerContainer "github.com/moby/moby/api/types/container"
	"go.podman.io/podman/v6/pkg/api/handlers"
	"go.podman.io/podman/v6/pkg/bindings"
	"go.podman.io/podman/v6/pkg/bindings/containers"
)

// execOutputLimit caps how much of an exec's stdout is kept for an error.
const execOutputLimit = 4096

// Exec runs cmd inside the running container id with stdin (when not nil)
// streamed to it until EOF, and returns the command's exit code. Output is
// discarded apart from a bounded tail quoted in errors; stderr is logged at
// debug level like an attached container's.
//
// Like Attach, the binding hijacks a socket, so the exec gets its own
// private bindings connection that is torn down when it returns. A session
// that cannot be started or inspected is an error; a non-zero exit is not.
func (e *Engine) Exec(ctx context.Context, id string, cmd []string, stdin io.Reader) (int, error) {
	dedicated, err := bindings.NewConnection(context.WithoutCancel(e.connCtx), e.socketURL)
	if err != nil {
		return 0, mapErr(err)
	}
	tracker := &connTracker{}
	if client, err := bindings.GetClient(dedicated); err == nil {
		if tr, ok := client.Client.Transport.(*http.Transport); ok && tr.DialContext != nil {
			tr = tr.Clone()
			tr.DialContext = tracker.wrap(tr.DialContext)
			client.Client.Transport = tr
		}
	}
	defer tracker.closeAll()
	c, cancel := context.WithCancel(dedicated)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()

	sessID, err := containers.ExecCreate(c, id, &handlers.ExecCreateConfig{
		ExecCreateRequest: dockerContainer.ExecCreateRequest{
			Cmd:          cmd,
			AttachStdin:  stdin != nil,
			AttachStdout: true,
			AttachStderr: true,
			Tty:          false,
		},
	})
	if err != nil {
		return 0, mapErr(err)
	}
	defer func() {
		_ = containers.ExecRemove(context.WithoutCancel(c), sessID, new(containers.ExecRemoveOptions).WithForce(true))
	}()

	out := &tailWriter{limit: execOutputLimit}
	var stdout io.Writer = out
	stderr := stderrLogger(e.log, id)
	opts := new(containers.ExecStartAndAttachOptions).
		WithOutputStream(stdout).WithAttachOutput(true).
		WithErrorStream(stderr).WithAttachError(true)
	if stdin != nil {
		opts = opts.WithInputStream(*bufio.NewReader(stdin)).WithAttachInput(true)
	}
	if err := containers.ExecStartAndAttach(c, sessID, opts); err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, mapErr(err)
	}
	insp, err := containers.ExecInspect(c, sessID, nil)
	if err != nil {
		return 0, mapErr(err)
	}
	if insp.ExitCode != 0 && out.Len() > 0 {
		return insp.ExitCode, fmt.Errorf("exec exited with code %d: %s", insp.ExitCode, out.String())
	}
	return insp.ExitCode, nil
}

// tailWriter keeps the last limit bytes written to it.
type tailWriter struct {
	buf   bytes.Buffer
	limit int
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	if w.buf.Len() > w.limit {
		w.buf.Next(w.buf.Len() - w.limit)
	}
	return len(p), nil
}

func (w *tailWriter) Len() int       { return w.buf.Len() }
func (w *tailWriter) String() string { return string(bytes.TrimSpace(w.buf.Bytes())) }

// InspectConfig returns the environment, entrypoint and command Podman
// recorded for the container: what `podman inspect` shows. The integration
// tests use it to check that nothing secret is recorded there.
func (e *Engine) InspectConfig(ctx context.Context, id string) (env, entrypoint, cmd []string, err error) {
	c, done := e.ctx(ctx)
	defer done()
	d, err := containers.Inspect(c, id, nil)
	if err != nil {
		return nil, nil, nil, mapErr(err)
	}
	if d.Config == nil {
		return nil, nil, nil, nil
	}
	return d.Config.Env, d.Config.Entrypoint, d.Config.Cmd, nil
}
