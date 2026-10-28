// Package ops serves liveness, readiness, and Prometheus metrics endpoints.
package ops

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/kenmoini/mcp-machine/internal/store"
)

const (
	textContentType    = "text/plain; charset=utf-8"
	metricsContentType = "text/plain; version=0.0.4"
)

// Probe reports whether the service is ready to serve; nil means ready.
type Probe func(ctx context.Context) error

// Metrics is a point-in-time snapshot of the exported counters and gauges.
type Metrics struct {
	ServersByState     map[store.State]int
	ProxyCalls         map[string]uint64
	ConfigReloads      uint64
	ConfigReloadErrors uint64
}

// Mount registers GET /healthz, GET /readyz, and GET /metrics on mux. A nil
// ready probe means always ready; metrics supplies a fresh snapshot per scrape.
func Mount(mux *http.ServeMux, ready Probe, metrics func() Metrics) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeText(w, http.StatusOK, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if ready != nil {
			if err := ready(r.Context()); err != nil {
				writeText(w, http.StatusServiceUnavailable, err.Error())
				return
			}
		}
		writeText(w, http.StatusOK, "ok")
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		var m Metrics
		if metrics != nil {
			m = metrics()
		}
		w.Header().Set("Content-Type", metricsContentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(render(m)))
	})
}

func writeText(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", textContentType)
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}

// render formats m in the Prometheus text exposition format.
func render(m Metrics) string {
	var b strings.Builder

	b.WriteString("# HELP mcp_machine_servers Number of managed servers by lifecycle state.\n")
	b.WriteString("# TYPE mcp_machine_servers gauge\n")
	states := make([]string, 0, len(m.ServersByState))
	for s := range m.ServersByState {
		states = append(states, string(s))
	}
	sort.Strings(states)
	for _, s := range states {
		fmt.Fprintf(&b, "mcp_machine_servers{state=\"%s\"} %d\n", escapeLabel(s), m.ServersByState[store.State(s)])
	}

	b.WriteString("# HELP mcp_machine_proxy_calls_total Requests forwarded to a backend server.\n")
	b.WriteString("# TYPE mcp_machine_proxy_calls_total counter\n")
	names := make([]string, 0, len(m.ProxyCalls))
	for n := range m.ProxyCalls {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "mcp_machine_proxy_calls_total{server=\"%s\"} %d\n", escapeLabel(n), m.ProxyCalls[n])
	}

	b.WriteString("# HELP mcp_machine_config_reloads_total Configuration reloads attempted.\n")
	b.WriteString("# TYPE mcp_machine_config_reloads_total counter\n")
	fmt.Fprintf(&b, "mcp_machine_config_reloads_total %d\n", m.ConfigReloads)

	b.WriteString("# HELP mcp_machine_config_reload_errors_total Configuration reloads that failed.\n")
	b.WriteString("# TYPE mcp_machine_config_reload_errors_total counter\n")
	fmt.Fprintf(&b, "mcp_machine_config_reload_errors_total %d\n", m.ConfigReloadErrors)

	return b.String()
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// escapeLabel escapes a Prometheus label value.
func escapeLabel(s string) string { return labelEscaper.Replace(s) }
