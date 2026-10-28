package ops_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kenmoini/mcp-machine/internal/ops"
	"github.com/kenmoini/mcp-machine/internal/store"
)

func get(t *testing.T, mux *http.ServeMux, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func newMux(ready ops.Probe, metrics func() ops.Metrics) *http.ServeMux {
	mux := http.NewServeMux()
	ops.Mount(mux, ready, metrics)
	return mux
}

func TestHealthz200(t *testing.T) {
	mux := newMux(func(context.Context) error { return errors.New("not ready") }, func() ops.Metrics { return ops.Metrics{} })
	rec := get(t, mux, http.MethodGet, "/healthz")
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestReadyz200(t *testing.T) {
	mux := newMux(func(context.Context) error { return nil }, func() ops.Metrics { return ops.Metrics{} })
	rec := get(t, mux, http.MethodGet, "/readyz")
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestReadyz503WhenProbeFails(t *testing.T) {
	mux := newMux(func(context.Context) error { return errors.New("podman socket unreachable") }, func() ops.Metrics { return ops.Metrics{} })
	rec := get(t, mux, http.MethodGet, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "podman socket unreachable") {
		t.Fatalf("body=%q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type=%q", ct)
	}
}

func TestReadyzNilProbeIsReady(t *testing.T) {
	mux := newMux(nil, func() ops.Metrics { return ops.Metrics{} })
	if rec := get(t, mux, http.MethodGet, "/readyz"); rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestMetricsText(t *testing.T) {
	mux := newMux(nil, func() ops.Metrics {
		return ops.Metrics{
			ServersByState:     map[store.State]int{store.StateReady: 2, store.StateFailed: 1},
			ProxyCalls:         map[string]uint64{"zeta": 3, "alpha": 7},
			ConfigReloads:      5,
			ConfigReloadErrors: 1,
		}
	})
	rec := get(t, mux, http.MethodGet, "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4" {
		t.Fatalf("content-type=%q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"# HELP mcp_machine_servers ",
		"# TYPE mcp_machine_servers gauge\n",
		`mcp_machine_servers{state="ready"} 2` + "\n",
		`mcp_machine_servers{state="failed"} 1` + "\n",
		"# TYPE mcp_machine_proxy_calls_total counter\n",
		`mcp_machine_proxy_calls_total{server="alpha"} 7` + "\n",
		`mcp_machine_proxy_calls_total{server="zeta"} 3` + "\n",
		"# TYPE mcp_machine_config_reloads_total counter\n",
		"mcp_machine_config_reloads_total 5\n",
		"# TYPE mcp_machine_config_reload_errors_total counter\n",
		"mcp_machine_config_reload_errors_total 1\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q in:\n%s", want, body)
		}
	}
	if strings.Index(body, `state="failed"`) > strings.Index(body, `state="ready"`) {
		t.Errorf("states not sorted:\n%s", body)
	}
	if strings.Index(body, `server="alpha"`) > strings.Index(body, `server="zeta"`) {
		t.Errorf("servers not sorted:\n%s", body)
	}
}

func TestMetricsEscapesLabelValues(t *testing.T) {
	mux := newMux(nil, func() ops.Metrics {
		return ops.Metrics{ProxyCalls: map[string]uint64{"a\"b\\c\nd": 1}}
	})
	body := get(t, mux, http.MethodGet, "/metrics").Body.String()
	if want := `mcp_machine_proxy_calls_total{server="a\"b\\c\nd"} 1`; !strings.Contains(body, want) {
		t.Fatalf("missing %q in:\n%s", want, body)
	}
}

func TestMetricsEmptyStillHasReloadCounters(t *testing.T) {
	mux := newMux(nil, func() ops.Metrics { return ops.Metrics{} })
	body := get(t, mux, http.MethodGet, "/metrics").Body.String()
	if !strings.Contains(body, "mcp_machine_config_reloads_total 0\n") {
		t.Fatalf("body:\n%s", body)
	}
}

func TestPostIsMethodNotAllowed(t *testing.T) {
	mux := newMux(nil, func() ops.Metrics { return ops.Metrics{} })
	for _, p := range []string{"/healthz", "/readyz", "/metrics"} {
		if rec := get(t, mux, http.MethodPost, p); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s code=%d, want 405", p, rec.Code)
		}
	}
}
