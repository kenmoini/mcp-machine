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
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

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
	var out, errb syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- cli.Run(ctx, []string{"serve", "-c", cfgPath}, strings.NewReader(""), &out, &errb,
			cli.Options{NewEngine: fakeEngine})
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
