package podman

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"go.podman.io/podman/v6/libpod/define"
	"go.podman.io/podman/v6/pkg/errorhandling"

	"github.com/kenmoini/mcp-machine/internal/runtime"
)

func TestMapErr(t *testing.T) {
	if got := mapErr(nil); got != nil {
		t.Fatalf("mapErr(nil) = %v, want nil", got)
	}

	nf := &errorhandling.ErrorModel{Message: "no such container", ResponseCode: http.StatusNotFound}
	got := mapErr(nf)
	if !errors.Is(got, runtime.ErrNotFound) {
		t.Fatalf("404 should map to ErrNotFound, got %v", got)
	}
	if !strings.Contains(got.Error(), "no such container") {
		t.Errorf("mapped error lost the podman message: %q", got)
	}

	// A wrapped 404 is still found through errors.As.
	if got := mapErr(fmt.Errorf("stop: %w", nf)); !errors.Is(got, runtime.ErrNotFound) {
		t.Errorf("wrapped 404 should map to ErrNotFound, got %v", got)
	}

	for _, code := range []int{http.StatusConflict, http.StatusInternalServerError, http.StatusBadRequest} {
		in := &errorhandling.ErrorModel{Message: "x", ResponseCode: code}
		out := mapErr(in)
		if out != error(in) { //nolint:errorlint // identity is the contract
			t.Errorf("code %d: error should pass through unchanged, got %v", code, out)
		}
		if errors.Is(out, runtime.ErrNotFound) {
			t.Errorf("code %d must not map to ErrNotFound", code)
		}
	}

	plain := errors.New("boom")
	if out := mapErr(plain); out != plain { //nolint:errorlint // identity is the contract
		t.Errorf("non-ErrorModel error should pass through, got %v", out)
	}
}

func TestInspectPorts(t *testing.T) {
	if got := inspectPorts(nil); got != nil {
		t.Fatalf("inspectPorts(nil) = %v, want nil", got)
	}

	in := map[string][]define.InspectHostPort{
		"3000/tcp":  {{HostIP: "127.0.0.1", HostPort: "49152"}},
		"53/udp":    {{HostIP: "", HostPort: "5353"}, {HostIP: "::1", HostPort: "5354"}},
		"8080/tcp":  {{HostIP: "0.0.0.0", HostPort: ""}},     // empty host port: skipped
		"9000/tcp":  {{HostIP: "0.0.0.0", HostPort: "http"}}, // non-numeric host port: skipped
		"bogus/tcp": {{HostIP: "0.0.0.0", HostPort: "1234"}}, // malformed key: skipped
		"/tcp":      {{HostIP: "0.0.0.0", HostPort: "1235"}}, // empty container port: skipped
		"7000":      {{HostIP: "1.2.3.4", HostPort: "7001"}}, // no protocol: kept, empty proto
		"6000/tcp":  nil,                                     // no bindings: nothing emitted
		"80/udp":    {{HostIP: "0.0.0.0", HostPort: "8081"}},
		"80/tcp": { // ties on container port and protocol break by host IP, then host port
			{HostIP: "127.0.0.1", HostPort: "8082"},
			{HostIP: "0.0.0.0", HostPort: "8090"},
			{HostIP: "127.0.0.1", HostPort: "8080"},
		},
	}
	got := inspectPorts(in)
	// inspectPorts sorts by container port, protocol, host IP, host port, so
	// the result is compared as returned, with no sorting here.
	want := []runtime.PortMapping{
		{HostIP: "", HostPort: 5353, ContainerPort: 53, Protocol: "udp"},
		{HostIP: "::1", HostPort: 5354, ContainerPort: 53, Protocol: "udp"},
		{HostIP: "0.0.0.0", HostPort: 8090, ContainerPort: 80, Protocol: "tcp"},
		{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
		{HostIP: "127.0.0.1", HostPort: 8082, ContainerPort: 80, Protocol: "tcp"},
		{HostIP: "0.0.0.0", HostPort: 8081, ContainerPort: 80, Protocol: "udp"},
		{HostIP: "127.0.0.1", HostPort: 49152, ContainerPort: 3000, Protocol: "tcp"},
		{HostIP: "1.2.3.4", HostPort: 7001, ContainerPort: 7000, Protocol: ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("inspectPorts =\n %+v\nwant\n %+v", got, want)
	}
}

func TestSortedNetworkNames(t *testing.T) {
	if got := sortedNetworkNames(nil); got != nil {
		t.Fatalf("nil map = %v, want nil", got)
	}
	if got := sortedNetworkNames(map[string]*define.InspectAdditionalNetwork{}); got != nil {
		t.Fatalf("empty map = %v, want nil", got)
	}
	m := map[string]*define.InspectAdditionalNetwork{"zeta": nil, "alpha": {}, "mcp-machine": nil, "beta": nil}
	want := []string{"alpha", "beta", "mcp-machine", "zeta"}
	if got := sortedNetworkNames(m); !reflect.DeepEqual(got, want) {
		t.Fatalf("sortedNetworkNames = %v, want %v", got, want)
	}
}

// feed sends each step on its channel in order, then closes both channels.
// The channels are unbuffered, so collectLogs observes the steps in exactly
// this order.
type logStep struct {
	stderr bool
	text   string
}

func collect(steps ...logStep) []runtime.LogLine {
	stdout := make(chan string)
	stderr := make(chan string)
	go func() {
		for _, s := range steps {
			if s.stderr {
				stderr <- s.text
			} else {
				stdout <- s.text
			}
		}
		close(stdout)
		close(stderr)
	}()
	return collectLogs(stdout, stderr)
}

func TestCollectLogs(t *testing.T) {
	so := func(s string) logStep { return logStep{text: s} }
	se := func(s string) logStep { return logStep{stderr: true, text: s} }
	ll := func(stream, text string) runtime.LogLine { return runtime.LogLine{Stream: stream, Text: text} }

	for _, tc := range []struct {
		name  string
		steps []logStep
		want  []runtime.LogLine
	}{
		{"empty", nil, nil},
		{"stdout and stderr lines", []logStep{so("a\n"), se("b\n")}, []runtime.LogLine{ll("stdout", "a"), ll("stderr", "b")}},
		{"multiple lines per frame", []logStep{so("a\nb\nc\n")}, []runtime.LogLine{ll("stdout", "a"), ll("stdout", "b"), ll("stdout", "c")}},
		{"line split across frames", []logStep{so("hel"), so("lo wor"), so("ld\n")}, []runtime.LogLine{ll("stdout", "hello world")}},
		{"crlf trimmed", []logStep{so("a\r\nb\r\n")}, []runtime.LogLine{ll("stdout", "a"), ll("stdout", "b")}},
		{"empty lines kept", []logStep{so("\n\nx\n")}, []runtime.LogLine{ll("stdout", ""), ll("stdout", ""), ll("stdout", "x")}},
		{"trailing partial stdout", []logStep{so("a\npart")}, []runtime.LogLine{ll("stdout", "a"), ll("stdout", "part")}},
		{"trailing partial stderr", []logStep{se("tail")}, []runtime.LogLine{ll("stderr", "tail")}},
		{
			"partial lines are per stream and flush stdout then stderr",
			[]logStep{se("e1"), so("o1"), se("-more"), so("-more")},
			[]runtime.LogLine{ll("stdout", "o1-more"), ll("stderr", "e1-more")},
		},
		{
			"interleaved order preserved",
			[]logStep{so("1\n"), se("2\n"), so("3\n"), se("4\n"), so("5\n")},
			[]runtime.LogLine{ll("stdout", "1"), ll("stderr", "2"), ll("stdout", "3"), ll("stderr", "4"), ll("stdout", "5")},
		},
		{
			"partial completed after another stream's line",
			[]logStep{so("ab"), se("x\n"), so("c\n")},
			[]runtime.LogLine{ll("stderr", "x"), ll("stdout", "abc")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := collect(tc.steps...); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("collectLogs = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestCollectLogsNilChannels(t *testing.T) {
	// Both channels already nil: nothing to drain, returns immediately.
	if got := collectLogs(nil, nil); got != nil {
		t.Fatalf("collectLogs(nil, nil) = %v, want nil", got)
	}
}
