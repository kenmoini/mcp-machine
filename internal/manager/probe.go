package manager

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// probeInterval is the pause between readiness probe attempts.
const probeInterval = 500 * time.Millisecond

// probeRequestTimeout bounds a single probe request.
const probeRequestTimeout = 2 * time.Second

// probe polls url with GET until the server answers or timeout elapses. Any
// HTTP response, whatever its status, means the server is up: an MCP endpoint
// commonly rejects a bare GET. Connection errors are retried every interval.
// If ctx ends first, probe returns ctx's error.
func probe(ctx context.Context, url string, timeout, interval time.Duration) error {
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := &http.Client{
		Timeout: probeRequestTimeout,
		// A fresh transport: no idle connections outlive the probe.
		Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DisableKeepAlives: true},
		// A redirect is an answer; do not chase it.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	for {
		req, err := http.NewRequestWithContext(pctx, http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("probe %s: %w", url, err)
		}
		resp, err := client.Do(req)
		if err == nil {
			// Do not read the body: an SSE endpoint answers GET with a
			// stream that never ends.
			_ = resp.Body.Close()
			return nil
		}
		t := time.NewTimer(interval)
		select {
		case <-pctx.Done():
			t.Stop()
			if err := ctx.Err(); err != nil {
				return err
			}
			return fmt.Errorf("probe %s: timed out after %s", url, timeout)
		case <-t.C:
		}
	}
}
