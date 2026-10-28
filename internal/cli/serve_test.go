package cli_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kenmoini/mcp-machine/internal/cli"
	"github.com/kenmoini/mcp-machine/internal/runtime"
	"github.com/kenmoini/mcp-machine/internal/runtime/fake"
)

// syncBuffer is a bytes.Buffer safe for concurrent use.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func fakeEngine(context.Context, string) (runtime.Engine, error) { return fake.New(), nil }

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const dupConfig = `servers:
  - {name: dup, type: remote, remote: {url: "http://x/mcp"}}
  - {name: dup, type: remote, remote: {url: "http://y/mcp"}}
`

var listeningRE = regexp.MustCompile(`listening on (https?://\S+)`)

// startServe runs "serve" in a goroutine and returns the printed base URL and
// a channel that yields the exit code.
func startServe(t *testing.T, ctx context.Context, cfgPath string) (string, <-chan int, *syncBuffer) {
	t.Helper()
	return startServeWith(t, ctx, cfgPath, fakeEngine)
}

// startServeWith is startServe with a caller-supplied engine factory.
func startServeWith(t *testing.T, ctx context.Context, cfgPath string,
	newEngine func(context.Context, string) (runtime.Engine, error)) (string, <-chan int, *syncBuffer) {
	t.Helper()
	var out, errb syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- cli.Run(ctx, []string{"serve", "-c", cfgPath}, strings.NewReader(""), &out, &errb,
			cli.Options{NewEngine: newEngine})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m := listeningRE.FindStringSubmatch(out.String()); m != nil {
			return m[1], done, &errb
		}
		select {
		case code := <-done:
			t.Fatalf("serve exited early with %d; stderr=%q", code, errb.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatalf("no listening line; stdout=%q stderr=%q", out.String(), errb.String())
	return "", nil, nil
}

func waitExit(t *testing.T, done <-chan int, errb *syncBuffer) {
	t.Helper()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code = %d, want 0; stderr=%q", code, errb.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop within 5s of cancel")
	}
}

func TestServeHealthzAndShutdown(t *testing.T) {
	path := writeConfig(t, "listen: 127.0.0.1:0\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base, done, errb := startServe(t, ctx, path)

	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d", resp.StatusCode)
	}
	if !strings.HasPrefix(base, "http://127.0.0.1:") {
		t.Fatalf("base = %q", base)
	}

	cancel()
	waitExit(t, done, errb)
}

func selfSignedCert(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func TestServeTLS(t *testing.T) {
	certFile, keyFile := selfSignedCert(t)
	path := writeConfig(t, fmt.Sprintf("listen: 127.0.0.1:0\ntls:\n  cert_file: %s\n  key_file: %s\n", certFile, keyFile))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base, done, errb := startServe(t, ctx, path)
	if !strings.HasPrefix(base, "https://") {
		t.Fatalf("base = %q, want https", base)
	}

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // self-signed test cert
	resp, err := client.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d", resp.StatusCode)
	}

	cancel()
	waitExit(t, done, errb)
}

func TestServeInvalidConfigExits1(t *testing.T) {
	path := writeConfig(t, dupConfig)
	var out, errb bytes.Buffer
	code := cli.Run(context.Background(), []string{"serve", "-c", path}, strings.NewReader(""), &out, &errb,
		cli.Options{NewEngine: fakeEngine})
	if code != 1 || !strings.Contains(errb.String(), "dup") {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
}

func TestServeEngineErrorExits1(t *testing.T) {
	path := writeConfig(t, "listen: 127.0.0.1:0\n")
	var out, errb bytes.Buffer
	code := cli.Run(context.Background(), []string{"serve", "-c", path}, strings.NewReader(""), &out, &errb,
		cli.Options{NewEngine: func(context.Context, string) (runtime.Engine, error) {
			return nil, errors.New("no podman here")
		}})
	if code != 1 || !strings.Contains(errb.String(), "no podman here") {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
}

func TestServeMissingConfigExits1(t *testing.T) {
	var out, errb bytes.Buffer
	code := cli.Run(context.Background(), []string{"serve", "-c", filepath.Join(t.TempDir(), "nope.yaml")},
		strings.NewReader(""), &out, &errb, cli.Options{NewEngine: fakeEngine})
	if code != 1 || errb.Len() == 0 {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
}

func TestValidateCommand(t *testing.T) {
	path := writeConfig(t, "servers:\n  - {name: a, type: remote, remote: {url: \"http://x/mcp\"}}\n")
	code, out, errs := run(t, "validate", "-c", path)
	if code != 0 || strings.TrimSpace(out) != "ok" || errs != "" {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
}

func TestValidateCommandInvalid(t *testing.T) {
	path := writeConfig(t, dupConfig)
	code, out, errs := run(t, "validate", "-c", path)
	if code != 1 || out != "" || !strings.Contains(errs, "dup") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
}

// pingBackend serves an MCP server with one tool, "ping", over Streamable HTTP.
func pingBackend(t *testing.T) *httptest.Server {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "1"}, nil)
	srv.AddTool(&mcp.Tool{Name: "ping", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil
		})
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true}))
	t.Cleanup(ts.Close)
	return ts
}

// toolNames connects an SDK client to url and lists the tool names.
func toolNames(ctx context.Context, url string) ([]string, error) {
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cs.Close() }()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names, nil
}

// statusOf returns the HTTP status of a request, or 0 on a transport error.
func statusOf(t *testing.T, method, url, body string) int {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func getBody(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestServeReconcilesAndReloads(t *testing.T) {
	backend := pingBackend(t)
	path := writeConfig(t, fmt.Sprintf("listen: 127.0.0.1:0\nservers:\n  - {name: a, type: remote, remote: {url: %q}}\n", backend.URL+"/"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base, done, errb := startServe(t, ctx, path)

	// The server becomes ready asynchronously after the first Apply.
	var names []string
	var lastErr error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cctx, ccancel := context.WithTimeout(ctx, time.Second)
		names, lastErr = toolNames(cctx, base+"/servers/a/mcp")
		ccancel()
		if lastErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr != nil || len(names) != 1 || names[0] != "ping" {
		t.Fatalf("tools = %v, err = %v; stderr=%q", names, lastErr, errb.String())
	}

	// Removing the server from the file stops serving it.
	if err := os.WriteFile(path, []byte("listen: 127.0.0.1:0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const probe = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	deadline = time.Now().Add(5 * time.Second)
	got := 0
	for time.Now().Before(deadline) {
		if got = statusOf(t, http.MethodPost, base+"/servers/a/mcp", probe); got == http.StatusNotFound {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got != http.StatusNotFound {
		t.Fatalf("POST after removal = %d, want 404; stderr=%q", got, errb.String())
	}
	_, metrics := getBody(t, base+"/metrics")
	if !regexp.MustCompile(`mcp_machine_config_reloads_total [1-9]`).MatchString(metrics) {
		t.Errorf("metrics lack a counted reload:\n%s", metrics)
	}

	cancel()
	waitExit(t, done, errb)
}

func TestServeReadyzWaitsForApply(t *testing.T) {
	eng := fake.New()
	eng.FailNext("ListContainers", errors.New("engine hiccup"))
	path := writeConfig(t, "listen: 127.0.0.1:0\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base, done, errb := startServeWith(t, ctx, path, func(context.Context, string) (runtime.Engine, error) { return eng, nil })

	if code, body := getBody(t, base+"/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz before first Apply = %d %q, want 503", code, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	code := 0
	for time.Now().Before(deadline) {
		if code, _ = getBody(t, base+"/readyz"); code == http.StatusOK {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if code != http.StatusOK {
		t.Fatalf("readyz never became ready; stderr=%q", errb.String())
	}

	cancel()
	waitExit(t, done, errb)
}

func TestServeReloadErrorIsRecorded(t *testing.T) {
	path := writeConfig(t, "listen: 127.0.0.1:0\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base, done, errb := startServe(t, ctx, path)

	if err := os.WriteFile(path, []byte("servers: [this is: not valid: yaml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var metrics string
	for time.Now().Before(deadline) {
		if _, metrics = getBody(t, base+"/metrics"); strings.Contains(metrics, "mcp_machine_config_reload_errors_total 1\n") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(metrics, "mcp_machine_config_reload_errors_total 1\n") {
		t.Fatalf("reload error not counted:\n%s", metrics)
	}

	cancel()
	waitExit(t, done, errb)
}
