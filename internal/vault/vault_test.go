package vault

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kenmoini/mcp-machine/internal/config"
)

// fakeVault answers requests in memory, without a listener (the sandbox
// cannot bind ports). Handlers are keyed by "METHOD /v1/path".
type fakeVault struct {
	mu       sync.Mutex
	handlers map[string]func(r *http.Request) (int, any)
	requests []*http.Request
}

func newFakeVault() *fakeVault {
	return &fakeVault{handlers: map[string]func(*http.Request) (int, any){}}
}

func (f *fakeVault) on(key string, h func(*http.Request) (int, any)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[key] = h
}

func (f *fakeVault) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.requests = append(f.requests, r)
	h, ok := f.handlers[r.Method+" "+r.URL.Path]
	f.mu.Unlock()
	status, body := http.StatusNotFound, any(map[string]any{"errors": []string{}})
	if ok {
		status, body = h(r)
	}
	b, _ := json.Marshal(body)
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(b))),
		Request:    r,
	}, nil
}

func (f *fakeVault) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r.Method+" "+r.URL.Path == key {
			n++
		}
	}
	return n
}

func (f *fakeVault) last() *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

func newClient(t *testing.T, fv *fakeVault, c Config) *Client {
	t.Helper()
	if c.Address == "" {
		c.Address = "https://vault.test:8200"
	}
	cl, err := New(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cl.http.Transport = fv
	return cl
}

func kv2(data map[string]any) func(*http.Request) (int, any) {
	return func(*http.Request) (int, any) {
		return 200, map[string]any{"data": map[string]any{"data": data, "metadata": map[string]any{"version": 2}}}
	}
}

func TestReadKVv2Shape(t *testing.T) {
	fv := newFakeVault()
	fv.on("GET /v1/kv/data/github", kv2(map[string]any{"token": "ghp_x"}))
	cl := newClient(t, fv, Config{Token: "t"})
	data, err := cl.Read(context.Background(), "kv/data/github")
	if err != nil {
		t.Fatal(err)
	}
	if data["token"] != "ghp_x" || len(data) != 1 {
		t.Errorf("data = %v", data)
	}
	req := fv.last()
	if req.Header.Get("X-Vault-Token") != "t" || req.Header.Get("X-Vault-Request") != "true" || req.Header.Get("X-Vault-Namespace") != "" {
		t.Errorf("headers = %v", req.Header)
	}
}

func TestReadKVv1Shape(t *testing.T) {
	fv := newFakeVault()
	fv.on("GET /v1/kv/github", func(*http.Request) (int, any) {
		return 200, map[string]any{"data": map[string]any{"token": "v1", "data": "not-a-map"}}
	})
	cl := newClient(t, fv, Config{Token: "t"})
	data, err := cl.Read(context.Background(), "kv/github")
	if err != nil {
		t.Fatal(err)
	}
	if data["token"] != "v1" || data["data"] != "not-a-map" {
		t.Errorf("data = %v", data)
	}
}

func TestReadForbiddenError(t *testing.T) {
	fv := newFakeVault()
	fv.on("GET /v1/kv/data/secret", func(*http.Request) (int, any) {
		return 403, map[string]any{"errors": []string{"1 error occurred:", "permission denied"}}
	})
	cl := newClient(t, fv, Config{Token: "t"})
	_, err := cl.Read(context.Background(), "kv/data/secret")
	var re *ReadError
	if !errors.As(err, &re) || re.Status != 403 {
		t.Fatalf("err = %v, want a 403 ReadError", err)
	}
	if want := "vault: read kv/data/secret: 403 1 error occurred:; permission denied"; err.Error() != want {
		t.Errorf("err = %q, want %q", err, want)
	}

	_, err = cl.Read(context.Background(), "kv/data/missing")
	if err == nil || err.Error() != "vault: read kv/data/missing: 404 not found" {
		t.Errorf("404: %v", err)
	}
}

func TestNamespaceHeader(t *testing.T) {
	fv := newFakeVault()
	fv.on("GET /v1/kv/data/a", kv2(map[string]any{"k": "v"}))
	cl := newClient(t, fv, Config{Token: "t", Namespace: "team"})
	if _, err := cl.Read(context.Background(), "kv/data/a"); err != nil {
		t.Fatal(err)
	}
	if got := fv.last().Header.Get("X-Vault-Namespace"); got != "team" {
		t.Errorf("namespace header = %q", got)
	}
}

func TestLookupMissingKeyAndNonString(t *testing.T) {
	f := &Fake{}
	f.Set("kv/data/a", "token", "x")
	f.Set("kv/data/a", "n", 3)
	ctx := context.Background()
	if v, err := Lookup(ctx, f, config.Ref{Path: "kv/data/a", Key: "token"}); err != nil || v != "x" {
		t.Errorf("Lookup = %q, %v", v, err)
	}
	_, err := Lookup(ctx, f, config.Ref{Path: "kv/data/a", Key: "nope"})
	if err == nil || err.Error() != `vault: kv/data/a has no key "nope"` {
		t.Errorf("missing key: %v", err)
	}
	_, err = Lookup(ctx, f, config.Ref{Path: "kv/data/a", Key: "n"})
	if err == nil || err.Error() != `vault: kv/data/a key "n" is not a string` {
		t.Errorf("non-string: %v", err)
	}
	_, err = Lookup(ctx, f, config.Ref{Path: "kv/data/zzz", Key: "k"})
	var re *ReadError
	if !errors.As(err, &re) || re.Status != http.StatusNotFound {
		t.Errorf("missing path: %v", err)
	}
}

func TestResolveComposedAndCached(t *testing.T) {
	f := &Fake{}
	f.Set("kv/data/a", "user", "u")
	f.Set("kv/data/a", "pass", "p")
	f.Set("kv/data/b", "host", "h")
	ctx := context.Background()
	got, err := Resolve(ctx, f, "${vault:kv/data/a#user}:${vault:kv/data/a#pass}@${vault:kv/data/b#host} ${VAR}")
	if err != nil {
		t.Fatal(err)
	}
	if got != "u:p@h ${VAR}" {
		t.Errorf("got %q", got)
	}
	if reads := f.Reads(); len(reads) != 2 {
		t.Errorf("reads = %v, want one per path", reads)
	}

	m, err := ResolveMap(ctx, f, map[string]string{"A": "${vault:kv/data/a#user}", "B": "plain", "C": "${vault:kv/data/b#host}"})
	if err != nil {
		t.Fatal(err)
	}
	if m["A"] != "u" || m["B"] != "plain" || m["C"] != "h" {
		t.Errorf("map = %v", m)
	}
	if m, err := ResolveMap(ctx, f, nil); err != nil || m != nil {
		t.Errorf("nil map: %v %v", m, err)
	}

	_, err = Resolve(ctx, f, "x ${vault:kv/data/a#nope} y")
	if err == nil || !strings.Contains(err.Error(), "${vault:kv/data/a#nope}") || !strings.Contains(err.Error(), `has no key "nope"`) {
		t.Errorf("err = %v", err)
	}
	f.Fail(errors.New("down"))
	if _, err := ResolveMap(ctx, f, map[string]string{"A": "${vault:kv/data/a#user}"}); err == nil || !strings.Contains(err.Error(), "down") {
		t.Errorf("failing reader: %v", err)
	}
}

func TestLookupSelfAtStart(t *testing.T) {
	fv := newFakeVault()
	fv.on("GET /v1/auth/token/lookup-self", func(*http.Request) (int, any) {
		return 200, map[string]any{"data": map[string]any{"policies": []string{"default", "mcp"}, "ttl": 0, "renewable": false}}
	})
	cl := newClient(t, fv, Config{Token: "t"})
	if err := cl.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	cl.Close()
	if fv.count("POST /v1/auth/token/renew-self") != 0 {
		t.Error("a token without TTL was renewed")
	}

	fv.on("GET /v1/auth/token/lookup-self", func(*http.Request) (int, any) {
		return 403, map[string]any{"errors": []string{"permission denied"}}
	})
	cl = newClient(t, fv, Config{Token: "bad"})
	err := cl.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "vault: lookup token: 403 permission denied") {
		t.Errorf("bad token: %v", err)
	}
	cl.Close()
}

func TestAppRoleLoginAndRenew(t *testing.T) {
	fv := newFakeVault()
	var mu sync.Mutex
	logins, renews := 0, 0
	fv.on("POST /v1/auth/approle/login", func(r *http.Request) (int, any) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["role_id"] != "rid" || body["secret_id"] != "sid" {
			return 400, map[string]any{"errors": []string{"bad credentials"}}
		}
		mu.Lock()
		logins++
		n := logins
		mu.Unlock()
		return 200, map[string]any{"auth": map[string]any{"client_token": "tok" + strconv.Itoa(n), "lease_duration": 60, "renewable": true}}
	})
	fv.on("GET /v1/auth/token/lookup-self", func(*http.Request) (int, any) {
		return 200, map[string]any{"data": map[string]any{"policies": []string{"mcp"}, "ttl": 60, "renewable": true}}
	})
	fv.on("POST /v1/auth/token/renew-self", func(*http.Request) (int, any) {
		mu.Lock()
		defer mu.Unlock()
		renews++
		if renews == 2 {
			return 403, map[string]any{"errors": []string{"token expired"}}
		}
		return 200, map[string]any{"auth": map[string]any{"lease_duration": 60, "renewable": true}}
	})
	fv.on("GET /v1/kv/data/a", kv2(map[string]any{"k": "v"}))

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sid"), []byte("sid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cl := newClient(t, fv, Config{RoleID: "rid", SecretIDFile: filepath.Join(dir, "sid")})
	ticks := make(chan time.Time)
	var waits []time.Duration
	cl.after = func(d time.Duration) <-chan time.Time {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		return ticks
	}
	if err := cl.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cl.currentToken() != "tok1" {
		t.Fatalf("token = %q after login", cl.currentToken())
	}
	// First tick: a successful renewal.
	ticks <- time.Time{}
	// Second tick: renewal fails, the client logs in again.
	ticks <- time.Time{}
	// Third wait is pending: inspect.
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(waits)
		mu.Unlock()
		if n >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cl.Close()
	mu.Lock()
	defer mu.Unlock()
	if logins != 2 || renews != 2 {
		t.Errorf("logins = %d, renews = %d", logins, renews)
	}
	if cl.currentToken() != "tok2" {
		t.Errorf("token = %q, want the re-login's", cl.currentToken())
	}
	if len(waits) < 3 || waits[0] != 40*time.Second || waits[1] != 40*time.Second || waits[2] != 40*time.Second {
		t.Errorf("waits = %v, want 2/3 of the 60s lease each time", waits)
	}
	if got := fv.last().Header.Get("X-Vault-Token"); got != "tok2" && fv.last().URL.Path != "/v1/auth/approle/login" {
		t.Errorf("last request used token %q", got)
	}
}

func TestNewReadsFilesAndValidates(t *testing.T) {
	dir := t.TempDir()
	tf := filepath.Join(dir, "token")
	if err := os.WriteFile(tf, []byte(" hvs.file \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cl, err := New(Config{Address: "https://v:8200", TokenFile: tf}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cl.currentToken() != "hvs.file" {
		t.Errorf("token = %q", cl.currentToken())
	}
	if _, err := New(Config{Address: "https://v:8200", TokenFile: filepath.Join(dir, "nope")}, nil); err == nil || !strings.Contains(err.Error(), "read token file") {
		t.Errorf("missing file: %v", err)
	}
	if _, err := New(Config{Address: "vault:8200", Token: "t"}, nil); err == nil || !strings.Contains(err.Error(), "not an absolute http(s) URL") {
		t.Errorf("bad address: %v", err)
	}
	if _, err := New(Config{Address: "https://v:8200"}, nil); err == nil || !strings.Contains(err.Error(), "no token and no approle") {
		t.Errorf("no auth: %v", err)
	}
	if err := os.WriteFile(tf, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Address: "https://v:8200", TokenFile: tf}, nil); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Errorf("empty file: %v", err)
	}
	if _, err := New(Config{Address: "https://v:8200", Token: "t", CAFile: filepath.Join(dir, "nope.pem")}, nil); err == nil || !strings.Contains(err.Error(), "ca_file") {
		t.Errorf("missing ca: %v", err)
	}
}

func TestFromConfig(t *testing.T) {
	c := FromConfig(config.VaultConfig{
		Address: "https://v", Namespace: "n", CAFile: "/ca", TLSSkipVerify: true, Token: "t", TokenFile: "/t", Timeout: 3 * time.Second,
		AppRole: &config.VaultAppRole{RoleID: "r", RoleIDFile: "/r", SecretID: "s", SecretIDFile: "/s"},
	})
	want := Config{Address: "https://v", Namespace: "n", CAFile: "/ca", TLSSkipVerify: true, Token: "t", TokenFile: "/t", Timeout: 3 * time.Second,
		RoleID: "r", RoleIDFile: "/r", SecretID: "s", SecretIDFile: "/s"}
	if c != want {
		t.Errorf("FromConfig = %+v, want %+v", c, want)
	}
}
