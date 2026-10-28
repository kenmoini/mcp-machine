package podman

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"

	"go.podman.io/podman/v6/pkg/bindings"
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

// connTracker remembers every net.Conn dialed through a private bindings
// connection so Close can force-close the hijacked (101-upgraded) socket,
// which the binding detaches from its context and never closes itself.
type connTracker struct {
	mu     sync.Mutex
	conns  []net.Conn
	closed bool
}

type dialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// wrap returns a dialer that records the connections made by dial.
func (t *connTracker) wrap(dial dialFunc) dialFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		c, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.closed {
			_ = c.Close()
			return nil, net.ErrClosed
		}
		t.conns = append(t.conns, c)
		return c, nil
	}
}

// closeAll closes every recorded connection and any dialed afterwards.
func (t *connTracker) closeAll() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	for _, c := range t.conns {
		_ = c.Close()
	}
	t.conns = nil
}

// conn is the io.ReadWriteCloser returned by Attach: reads yield the
// container's stdout, writes feed its stdin.
type conn struct {
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
	stdinR  *io.PipeReader
	stdinW  *io.PipeWriter
	cancel  context.CancelFunc
	tracker *connTracker // may be nil
	once    sync.Once
}

func newConn(cancel context.CancelFunc, tracker *connTracker) *conn {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	return &conn{stdoutR: stdoutR, stdoutW: stdoutW, stdinR: stdinR, stdinW: stdinW, cancel: cancel, tracker: tracker}
}

func (c *conn) Read(p []byte) (int, error)  { return c.stdoutR.Read(p) }
func (c *conn) Write(p []byte) (int, error) { return c.stdinW.Write(p) }

// finish is called when the attach goroutine returns. Readers get EOF (or
// err), and writers fail fast instead of blocking on a stdin pipe that the
// binding no longer drains.
func (c *conn) finish(err error) {
	_ = c.stdoutW.CloseWithError(err)
	_ = c.stdinR.CloseWithError(io.ErrClosedPipe)
	_ = c.stdinW.CloseWithError(io.ErrClosedPipe)
}

// Close tears down the attachment and is safe to call more than once. It
// releases the stdin side immediately and force-closes the attach socket, so
// the binding's goroutine unwinds once its blocked socket read errors out.
func (c *conn) Close() error {
	c.once.Do(func() {
		c.cancel()
		c.tracker.closeAll()
		_ = c.stdinW.Close()
		_ = c.stdinR.Close()
		_ = c.stdoutW.Close()
		_ = c.stdoutR.Close()
	})
	return nil
}

// Attach connects to the container's stdio. It must be called before
// StartContainer: it returns once the attach stream is established.
//
// The binding's attach swaps the transport of the client it is given and
// hijacks a socket, so each attachment gets its own private bindings
// connection rather than the engine's shared one. The attachment outlives
// the caller's ctx (which may be a short-lived request ctx); it ends on
// Close or when the container's streams end. Close force-closes the
// attachment socket where the transport allows it, but the binding's read
// loop is not tied to any context, so callers should still stop or remove
// the container when tearing a session down.
func (e *Engine) Attach(ctx context.Context, id string) (io.ReadWriteCloser, error) {
	dedicated, err := bindings.NewConnection(context.WithoutCancel(e.connCtx), e.socketURL)
	if err != nil {
		return nil, mapErr(err)
	}
	tracker := &connTracker{}
	if client, err := bindings.GetClient(dedicated); err == nil {
		if tr, ok := client.Client.Transport.(*http.Transport); ok && tr.DialContext != nil {
			tr = tr.Clone()
			tr.DialContext = tracker.wrap(tr.DialContext)
			client.Client.Transport = tr
		}
	}

	attachCtx, cancel := context.WithCancel(dedicated)
	c := newConn(cancel, tracker)

	ready := make(chan bool, 1) // buffered so the binding never blocks on send
	errc := make(chan error, 1)
	go func() {
		err := containers.Attach(attachCtx, id, c.stdinR, c.stdoutW, stderrLogger(e.log, id), ready,
			&containers.AttachOptions{Stream: ptr(true)})
		c.finish(err)
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
