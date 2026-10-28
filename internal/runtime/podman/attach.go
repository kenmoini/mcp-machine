package podman

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync"

	"go.podman.io/podman/v6/pkg/bindings/containers"
)

// writerFunc adapts a function to io.Writer.
type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// stderrLogger returns a writer that logs each stderr line at Debug level.
// The binding calls Write sequentially from a single goroutine, so no locking
// is needed. Partial lines are buffered until a newline arrives.
func stderrLogger(log *slog.Logger, id string) io.Writer {
	var pending []byte
	return writerFunc(func(p []byte) (int, error) {
		pending = append(pending, p...)
		for {
			i := bytes.IndexByte(pending, '\n')
			if i < 0 {
				break
			}
			log.Debug("container stderr", "id", id, "line", string(bytes.TrimRight(pending[:i], "\r")))
			pending = pending[i+1:]
		}
		return len(p), nil
	})
}

// conn is the io.ReadWriteCloser returned by Attach: reads yield the
// container's stdout, writes feed its stdin.
type conn struct {
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
	stdinR  *io.PipeReader
	stdinW  *io.PipeWriter
	cancel  context.CancelFunc
	once    sync.Once
}

func (c *conn) Read(p []byte) (int, error)  { return c.stdoutR.Read(p) }
func (c *conn) Write(p []byte) (int, error) { return c.stdinW.Write(p) }

// Close tears down the attachment. It is safe to call more than once.
func (c *conn) Close() error {
	c.once.Do(func() {
		c.cancel()
		_ = c.stdinW.Close()
		_ = c.stdinR.Close()
		_ = c.stdoutW.Close()
		_ = c.stdoutR.Close()
	})
	return nil
}

// Attach connects to the container's stdio. It must be called before
// StartContainer: it returns once the attach stream is established.
func (e *Engine) Attach(ctx context.Context, id string) (io.ReadWriteCloser, error) {
	// The attachment outlives the caller's ctx (which may be a short-lived
	// request ctx), so derive from the connection ctx and cancel on Close.
	attachCtx, cancel := context.WithCancel(e.connCtx)

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	c := &conn{stdoutR: stdoutR, stdoutW: stdoutW, stdinR: stdinR, stdinW: stdinW, cancel: cancel}

	ready := make(chan bool, 1) // buffered so the binding never blocks on send
	errc := make(chan error, 1)
	go func() {
		err := containers.Attach(attachCtx, id, stdinR, stdoutW, stderrLogger(e.log, id), ready,
			&containers.AttachOptions{Stream: ptr(true)})
		// Readers get EOF (or err) once the attachment ends.
		_ = stdoutW.CloseWithError(err)
		errc <- err
	}()

	select {
	case <-ready:
		return c, nil
	case err := <-errc:
		_ = c.Close()
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return nil, mapErr(err)
	case <-ctx.Done():
		_ = c.Close()
		return nil, ctx.Err()
	}
}
