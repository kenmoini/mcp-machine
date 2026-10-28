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
	"html"
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
	"github.com/kenmoini/mcp-machine/internal/store"
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
func startServe(ctx context.Context, t *testing.T, cfgPath string) (string, <-chan int, *syncBuffer) {
	t.Helper()
	return startServeWith(ctx, t, cfgPath, fakeEngine)
}

// startServeWith is startServe with a caller-supplied engine factory. The
// user drop-in directory is a fresh temp dir, so no test touches /data.
func startServeWith(ctx context.Context, t *testing.T, cfgPath string,
	newEngine func(context.Context, string) (runtime.Engine, error)) (string, <-chan int, *syncBuffer) {
	t.Helper()
	return startServeArgs(ctx, t, serveArgs(t, cfgPath)[1:], newEngine)
}

// serveArgs is "serve -c cfgPath" with a fresh temp user drop-in directory.
func serveArgs(t *testing.T, cfgPath string) []string {
	t.Helper()
	return []string{"serve", "-c", cfgPath, "--config-d", filepath.Join(t.TempDir(), "config.d")}
}

// startServeArgs runs "serve" with args (the flags after the subcommand) in a
// goroutine and returns the printed base URL, a channel that yields the exit
// code, and stderr.
func startServeArgs(ctx context.Context, t *testing.T, args []string,
	newEngine func(context.Context, string) (runtime.Engine, error)) (string, <-chan int, *syncBuffer) {
	t.Helper()
	var out, errb syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- cli.Run(ctx, append([]string{"serve"}, args...), strings.NewReader(""), &out, &errb,
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
	base, done, errb := startServe(ctx, t, path)

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
	base, done, errb := startServe(ctx, t, path)
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
	code := cli.Run(context.Background(), serveArgs(t, path), strings.NewReader(""), &out, &errb,
		cli.Options{NewEngine: fakeEngine})
	if code != 1 || !strings.Contains(errb.String(), "dup") {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
}

func TestServeEngineErrorExits1(t *testing.T) {
	path := writeConfig(t, "listen: 127.0.0.1:0\n")
	var out, errb bytes.Buffer
	code := cli.Run(context.Background(), serveArgs(t, path), strings.NewReader(""), &out, &errb,
		cli.Options{NewEngine: func(context.Context, string) (runtime.Engine, error) {
			return nil, errors.New("no podman here")
		}})
	if code != 1 || !strings.Contains(errb.String(), "no podman here") {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
}

func TestServeNoConfigStartsWithWarning(t *testing.T) {
	tmp := t.TempDir()
	legacy := filepath.Join(tmp, "data", "config.yaml") // never created
	cli.SetLegacyConfigPath(t, legacy)
	root, user := filepath.Join(tmp, "nonexistent"), filepath.Join(tmp, "config.d")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base, done, errb := startServeArgs(ctx, t,
		[]string{"-c", root, "--config-d", user, "-listen", "127.0.0.1:0"}, fakeEngine)

	if code, _ := getBody(t, base+"/healthz"); code != http.StatusOK {
		t.Fatalf("healthz = %d", code)
	}
	want := "no configuration found (searched: " + strings.Join([]string{
		filepath.Join(root, "config.yaml"), filepath.Join(root, "config.d"), legacy, user,
	}, ", ") + "); running with defaults"
	if !strings.Contains(errb.String(), want) {
		t.Errorf("stderr lacks %q:\n%s", want, errb.String())
	}
	// The config page shows the same warning (HTML-escaped).
	if code, body := getBody(t, base+"/config"); code != http.StatusOK || !strings.Contains(body, html.EscapeString(want)) {
		t.Errorf("GET /config = %d, page lacks %q:\n%s", code, want, body)
	}

	cancel()
	waitExit(t, done, errb)
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
	base, done, errb := startServe(ctx, t, path)

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
	base, done, errb := startServeWith(ctx, t, path, func(context.Context, string) (runtime.Engine, error) { return eng, nil })

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
	base, done, errb := startServe(ctx, t, path)

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

func TestListenOrTLSFailureStartsNothing(t *testing.T) {
	// The listener and the TLS key pair come first: when either fails, no
	// engine is connected and no manager starts containers.
	dir := t.TempDir()
	for name, body := range map[string]string{
		"bad listen address": "listen: 127.0.0.1:not-a-port\n",
		"missing key pair": fmt.Sprintf("listen: 127.0.0.1:0\ntls:\n  cert_file: %s\n  key_file: %s\n",
			filepath.Join(dir, "missing.crt"), filepath.Join(dir, "missing.key")),
	} {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, body)
			engines := 0
			var out, errb bytes.Buffer
			code := cli.Run(context.Background(), serveArgs(t, path), strings.NewReader(""), &out, &errb,
				cli.Options{NewEngine: func(context.Context, string) (runtime.Engine, error) {
					engines++
					return fake.New(), nil
				}})
			if code != 1 {
				t.Fatalf("code = %d, want 1; stderr=%q", code, errb.String())
			}
			if engines != 0 {
				t.Errorf("engine created %d times before the listener failed", engines)
			}
			if strings.Contains(out.String(), "listening on") {
				t.Errorf("stdout = %q", out.String())
			}
		})
	}
}

// eventually polls cond every 50ms for up to 5s.
func eventually(t *testing.T, what string, errb *syncBuffer, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; stderr=%q", what, errb.String())
}

// waitServing waits until base serves server name with the ping tool.
func waitServing(ctx context.Context, t *testing.T, base, name string, errb *syncBuffer) {
	t.Helper()
	eventually(t, name+" serving ping", errb, func() bool {
		cctx, ccancel := context.WithTimeout(ctx, time.Second)
		defer ccancel()
		names, err := toolNames(cctx, base+"/servers/"+name+"/mcp")
		return err == nil && len(names) == 1 && names[0] == "ping"
	})
}

// writeAtomic writes body to path through a dot-file and a rename, so the
// config watcher never reads it half-written.
func writeAtomic(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func remoteDropIn(name, url string) string {
	return fmt.Sprintf("servers:\n  - {name: %s, type: remote, remote: {url: %q}}\n", name, url)
}

func TestServeRootModeLoadsDropIns(t *testing.T) {
	backend := pingBackend(t)
	root := t.TempDir()
	writeAtomic(t, filepath.Join(root, "config.yaml"), "listen: 127.0.0.1:0\n")
	writeAtomic(t, filepath.Join(root, "config.d", "a.yaml"), remoteDropIn("a", backend.URL+"/"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base, done, errb := startServeArgs(ctx, t, []string{"-c", root, "--config-d", filepath.Join(t.TempDir(), "config.d")}, fakeEngine)

	waitServing(ctx, t, base, "a", errb)

	cancel()
	waitExit(t, done, errb)
}

func TestServeHotReloadsUserDropIn(t *testing.T) {
	backend := pingBackend(t)
	path := writeConfig(t, "listen: 127.0.0.1:0\n")
	user := filepath.Join(t.TempDir(), "config.d")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base, done, errb := startServeArgs(ctx, t, []string{"-c", path, "--config-d", user}, fakeEngine)

	b := filepath.Join(user, "b.yaml")
	writeAtomic(t, b, remoteDropIn("b", backend.URL+"/"))
	waitServing(ctx, t, base, "b", errb)

	if err := os.Remove(b); err != nil {
		t.Fatal(err)
	}
	const probe = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	eventually(t, "b gone", errb, func() bool {
		return statusOf(t, http.MethodPost, base+"/servers/b/mcp", probe) == http.StatusNotFound
	})

	cancel()
	waitExit(t, done, errb)
}

func TestServeInvalidDropInKeepsLastGood(t *testing.T) {
	backend := pingBackend(t)
	root := t.TempDir()
	writeAtomic(t, filepath.Join(root, "config.yaml"),
		"listen: 127.0.0.1:0\n"+remoteDropIn("a", backend.URL+"/"))
	if err := os.MkdirAll(filepath.Join(root, "config.d"), 0o750); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base, done, errb := startServeArgs(ctx, t, []string{"-c", root, "--config-d", filepath.Join(t.TempDir(), "config.d")}, fakeEngine)
	waitServing(ctx, t, base, "a", errb)

	bad := filepath.Join(root, "config.d", "bad.yaml")
	writeAtomic(t, bad, "listen: :1\n")
	eventually(t, "reload failure on /config", errb, func() bool {
		_, body := getBody(t, base+"/config")
		return strings.Contains(body, "Last reload failed") && strings.Contains(body, bad)
	})
	waitServing(ctx, t, base, "a", errb)

	cancel()
	waitExit(t, done, errb)
}

func TestServeKeepsContainersWithoutConfig(t *testing.T) {
	tmp := t.TempDir()
	cli.SetLegacyConfigPath(t, filepath.Join(tmp, "data", "config.yaml"))
	eng := fake.New()
	eng.Seed(runtime.ContainerInfo{
		ID: "seed-ghost", Name: store.ContainerName("ghost"), Image: "img:1", State: "running",
		Labels: store.Labels("ghost", "whatever", "stdio"),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base, done, errb := startServeArgs(ctx, t,
		[]string{"-c", filepath.Join(tmp, "etc"), "--config-d", filepath.Join(tmp, "config.d"), "-listen", "127.0.0.1:0"},
		func(context.Context, string) (runtime.Engine, error) { return eng, nil })

	eventually(t, "readyz 200", errb, func() bool { code, _ := getBody(t, base+"/readyz"); return code == http.StatusOK })
	info, err := eng.InspectContainer(ctx, "seed-ghost")
	if err != nil {
		t.Fatalf("ghost container gone: %v", err)
	}
	if info.State != "running" {
		t.Errorf("ghost state = %q, want running", info.State)
	}
	for _, c := range eng.Calls() {
		if c == "StopContainer" || c == "RemoveContainer" {
			t.Errorf("engine call %s with no configuration loaded", c)
		}
	}

	cancel()
	waitExit(t, done, errb)
}

func TestServeLegacyFallbackWarns(t *testing.T) {
	backend := pingBackend(t)
	tmp := t.TempDir()
	legacy := filepath.Join(tmp, "data", "config.yaml")
	writeAtomic(t, legacy, "listen: 127.0.0.1:0\n"+remoteDropIn("a", backend.URL+"/"))
	cli.SetLegacyConfigPath(t, legacy)
	root := filepath.Join(tmp, "etc")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base, done, errb := startServeArgs(ctx, t, []string{"-c", root, "--config-d", filepath.Join(tmp, "config.d")}, fakeEngine)

	waitServing(ctx, t, base, "a", errb)
	want := "configuration read from the deprecated location " + legacy + "; move it to " + filepath.Join(root, "config.yaml")
	if !strings.Contains(errb.String(), want) {
		t.Errorf("stderr lacks %q:\n%s", want, errb.String())
	}

	cancel()
	waitExit(t, done, errb)
}

func TestServeReadOnlyUserDirDisablesEditing(t *testing.T) {
	path := writeConfig(t, "listen: 127.0.0.1:0\n")
	notDir := filepath.Join(t.TempDir(), "config.d")
	if err := os.WriteFile(notDir, []byte("a file, not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base, done, errb := startServeArgs(ctx, t, []string{"-c", path, "--config-d", notDir}, fakeEngine)

	code, body := getBody(t, base+"/")
	if code != http.StatusOK || strings.Contains(body, `href="/servers/new"`) {
		t.Errorf("GET / = %d, Add server link present: %v", code, strings.Contains(body, `href="/servers/new"`))
	}
	if code, _ := getBody(t, base+"/servers/new"); code != http.StatusServiceUnavailable {
		t.Errorf("GET /servers/new = %d, want 503", code)
	}
	if want := "editing is disabled: " + notDir + " is not writable ("; !strings.Contains(errb.String(), want) {
		t.Errorf("stderr lacks %q:\n%s", want, errb.String())
	}
	// Every page explains why editing is off.
	for _, page := range []string{"/", "/config"} {
		_, body := getBody(t, base+page)
		if want := "editing is disabled: " + html.EscapeString(notDir) + " is not writable ("; !strings.Contains(body, want) {
			t.Errorf("GET %s lacks %q:\n%s", page, want, body)
		}
	}

	cancel()
	waitExit(t, done, errb)
}

func TestValidateVerboseListsFilesAndOrigins(t *testing.T) {
	root := t.TempDir()
	base, dropIn := filepath.Join(root, "config.yaml"), filepath.Join(root, "config.d", "b.yaml")
	writeAtomic(t, base, remoteDropIn("a", "http://a/mcp"))
	writeAtomic(t, dropIn, remoteDropIn("b", "http://b/mcp"))
	code, out, errs := run(t, "validate", "-v", "-c", root, "--config-d", filepath.Join(root, "user.d"))
	want := "ok\nfiles:\n  " + base + "\n  " + dropIn + "\nservers:\n" +
		"  a: Defined in " + base + " (base)\n" +
		"  b: Defined in " + dropIn + " (system)\n"
	if code != 0 || errs != "" || out != want {
		t.Fatalf("code=%d err=%q out=%q\nwant %q", code, errs, out, want)
	}
}

func TestValidateNoConfigExits1(t *testing.T) {
	tmp := t.TempDir()
	legacy := filepath.Join(tmp, "data", "config.yaml")
	cli.SetLegacyConfigPath(t, legacy)
	root, user := filepath.Join(tmp, "etc"), filepath.Join(tmp, "user.d")
	code, out, errs := run(t, "validate", "-c", root, "--config-d", user)
	want := "mcp-machine validate: no configuration found (searched: " + strings.Join([]string{
		filepath.Join(root, "config.yaml"), filepath.Join(root, "config.d"), legacy, user,
	}, ", ") + ")\n"
	if code != 1 || out != "" || errs != want {
		t.Fatalf("code=%d out=%q err=%q\nwant %q", code, out, errs, want)
	}
}
