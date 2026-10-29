// Package vault is a small client for HashiCorp Vault and OpenBao: enough to
// log in with a token or an AppRole, keep the token renewed, and read the
// secrets that ${vault:PATH#KEY} references name. It speaks the HTTP API
// directly and needs no Vault library.
package vault

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/kenmoini/mcp-machine/internal/config"
)

// Reader reads a secret by its API path (under /v1/) and returns its data:
// for a KV v2 secret the key/value map, for anything else the response's
// data object. *Client and *Fake satisfy it.
type Reader interface {
	Read(ctx context.Context, path string) (map[string]any, error)
}

// Config says how to reach Vault and how to authenticate: a Token (or a
// TokenFile holding one), or an AppRole with a RoleID and a SecretID, each
// inline or in a file.
type Config struct {
	Address       string
	Namespace     string
	CAFile        string
	TLSSkipVerify bool
	Token         string
	TokenFile     string
	RoleID        string
	RoleIDFile    string
	SecretID      string
	SecretIDFile  string
	Timeout       time.Duration
}

// FromConfig converts the configuration's vault section.
func FromConfig(c config.VaultConfig) Config {
	out := Config{
		Address:       c.Address,
		Namespace:     c.Namespace,
		CAFile:        c.CAFile,
		TLSSkipVerify: c.TLSSkipVerify,
		Token:         c.Token,
		TokenFile:     c.TokenFile,
		Timeout:       c.Timeout,
	}
	if a := c.AppRole; a != nil {
		out.RoleID, out.RoleIDFile, out.SecretID, out.SecretIDFile = a.RoleID, a.RoleIDFile, a.SecretID, a.SecretIDFile
	}
	return out
}

// Client is a Vault client. Its token is renewed in the background once
// Start has run; Close stops that.
type Client struct {
	cfg  Config
	base string
	http *http.Client
	log  *slog.Logger

	mu    sync.Mutex
	token string

	roleID, secretID string

	// after is time.After; tests replace it.
	after func(time.Duration) <-chan time.Time

	cancel context.CancelFunc
	done   chan struct{}
}

var _ Reader = (*Client)(nil)

// ReadError is a failed secret read: an HTTP status from Vault, with its
// message, for example "vault: read kv/data/github: 403 permission denied".
type ReadError struct {
	Path   string
	Status int
	Msg    string
}

func (e *ReadError) Error() string {
	return fmt.Sprintf("vault: read %s: %d %s", e.Path, e.Status, e.Msg)
}

// New builds a client from c without talking to Vault. It reads the token,
// role ID and secret ID files, so a missing file fails here.
func New(c Config, log *slog.Logger) (*Client, error) {
	if log == nil {
		log = slog.Default()
	}
	u, err := url.Parse(c.Address)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("vault: address %q is not an absolute http(s) URL", c.Address)
	}
	if c.Timeout <= 0 {
		c.Timeout = 10 * time.Second
	}
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("vault: default transport is not an *http.Transport")
	}
	tr = tr.Clone()
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile) //nolint:gosec // G304: the operator's configured CA file
		if err != nil {
			return nil, fmt.Errorf("vault: read ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("vault: ca_file %s holds no certificate", c.CAFile)
		}
		tr.TLSClientConfig.RootCAs = pool
	}
	if c.TLSSkipVerify {
		tr.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec // G402: the operator asked for it (vault.tls_skip_verify)
	}
	cl := &Client{
		cfg:   c,
		base:  strings.TrimSuffix(c.Address, "/") + "/v1/",
		http:  &http.Client{Transport: tr, Timeout: c.Timeout},
		log:   log,
		after: time.After,
	}
	token, err := valueOrFile("token", c.Token, c.TokenFile)
	if err != nil {
		return nil, err
	}
	cl.token = token
	if cl.roleID, err = valueOrFile("approle.role_id", c.RoleID, c.RoleIDFile); err != nil {
		return nil, err
	}
	if cl.secretID, err = valueOrFile("approle.secret_id", c.SecretID, c.SecretIDFile); err != nil {
		return nil, err
	}
	if cl.token == "" && cl.roleID == "" {
		return nil, errors.New("vault: no token and no approle configured")
	}
	return cl, nil
}

// valueOrFile returns value, or the trimmed content of file when value is
// empty and file is set.
func valueOrFile(what, value, file string) (string, error) {
	if value != "" || file == "" {
		return value, nil
	}
	b, err := os.ReadFile(file) //nolint:gosec // G304: the operator's configured credential file
	if err != nil {
		return "", fmt.Errorf("vault: read %s file: %w", what, err)
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", fmt.Errorf("vault: %s file %s is empty", what, file)
	}
	return v, nil
}

// Start logs in (AppRole) when no token is set, checks the token with
// lookup-self, logging its policies and TTL, and starts renewing it in the
// background until Close. A failed check is returned; the client stays
// usable, so a Vault that is briefly down at startup only delays the first
// successful read.
func (c *Client) Start(ctx context.Context) error {
	ttl, renewable, err := c.authenticate(ctx)
	if err != nil {
		return err
	}
	bg, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c.cancel = cancel
	c.done = make(chan struct{})
	go c.renewLoop(bg, ttl, renewable)
	return nil
}

// authenticate obtains a token (AppRole) if the client has none, then looks
// it up and returns its TTL and whether it is renewable.
func (c *Client) authenticate(ctx context.Context) (ttl time.Duration, renewable bool, err error) {
	if c.currentToken() == "" {
		// The lookup below reports the token's TTL, so the login's is not needed.
		if _, _, err := c.login(ctx); err != nil {
			return 0, false, err
		}
	}
	var resp struct {
		Data struct {
			Policies    []string `json:"policies"`
			TTL         int64    `json:"ttl"`
			Renewable   bool     `json:"renewable"`
			DisplayName string   `json:"display_name"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "auth/token/lookup-self", nil, &resp); err != nil {
		return 0, false, fmt.Errorf("vault: lookup token: %w", err)
	}
	ttl = time.Duration(resp.Data.TTL) * time.Second
	c.log.Info("vault: authenticated", "address", c.cfg.Address, "token", resp.Data.DisplayName,
		"policies", strings.Join(resp.Data.Policies, ","), "ttl", ttl, "renewable", resp.Data.Renewable)
	return ttl, resp.Data.Renewable, nil
}

// login performs the AppRole login and installs the token.
func (c *Client) login(ctx context.Context) (ttl time.Duration, renewable bool, err error) {
	if c.roleID == "" {
		return 0, false, errors.New("vault: no token and no approle to log in with")
	}
	body := map[string]string{"role_id": c.roleID}
	if c.secretID != "" {
		body["secret_id"] = c.secretID
	}
	var resp struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int64  `json:"lease_duration"`
			Renewable     bool   `json:"renewable"`
		} `json:"auth"`
	}
	if err := c.do(ctx, http.MethodPost, "auth/approle/login", body, &resp); err != nil {
		return 0, false, fmt.Errorf("vault: approle login: %w", err)
	}
	if resp.Auth.ClientToken == "" {
		return 0, false, errors.New("vault: approle login returned no token")
	}
	c.setToken(resp.Auth.ClientToken)
	return time.Duration(resp.Auth.LeaseDuration) * time.Second, resp.Auth.Renewable, nil
}

// renew extends the token's lease and returns the new TTL.
func (c *Client) renew(ctx context.Context) (time.Duration, error) {
	var resp struct {
		Auth struct {
			LeaseDuration int64 `json:"lease_duration"`
		} `json:"auth"`
	}
	if err := c.do(ctx, http.MethodPost, "auth/token/renew-self", map[string]any{}, &resp); err != nil {
		return 0, fmt.Errorf("vault: renew token: %w", err)
	}
	return time.Duration(resp.Auth.LeaseDuration) * time.Second, nil
}

// renewRetry is the pause after a failed renewal or login.
const renewRetry = 10 * time.Second

// renewLoop renews the token at two thirds of its TTL. When a renewal fails
// it logs in again (AppRole) or, with a plain token, retries; a token with
// no TTL (a root or periodic one) needs no renewal.
func (c *Client) renewLoop(ctx context.Context, ttl time.Duration, renewable bool) {
	defer close(c.done)
	if ttl <= 0 || !renewable {
		return
	}
	wait := ttl * 2 / 3
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.after(wait):
		}
		next, err := c.renew(ctx)
		if err == nil {
			c.log.Debug("vault: token renewed", "ttl", next)
			wait = next * 2 / 3
			if wait <= 0 {
				return
			}
			continue
		}
		if ctx.Err() != nil {
			return
		}
		c.log.Warn("vault: token renewal failed", "err", err)
		if c.roleID != "" {
			if next, renewable, lerr := c.login(ctx); lerr == nil {
				c.log.Info("vault: logged in again", "ttl", next, "renewable", renewable)
				if next <= 0 || !renewable {
					return
				}
				wait = next * 2 / 3
				continue
			} else if ctx.Err() == nil {
				c.log.Warn("vault: approle login failed", "err", lerr)
			}
		}
		wait = renewRetry
	}
}

// Close stops the renewal loop.
func (c *Client) Close() {
	if c.cancel != nil {
		c.cancel()
		<-c.done
	}
}

func (c *Client) currentToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

func (c *Client) setToken(t string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = t
}

// Read returns the data of the secret at path. A KV v2 response (data.data
// with data.metadata) is unwrapped to the key/value map.
func (c *Client) Read(ctx context.Context, path string) (map[string]any, error) {
	var resp struct {
		Data map[string]any `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		var he *httpError
		if errors.As(err, &he) {
			return nil, &ReadError{Path: path, Status: he.status, Msg: he.msg}
		}
		return nil, fmt.Errorf("vault: read %s: %w", path, err)
	}
	if inner, ok := resp.Data["data"].(map[string]any); ok {
		if _, meta := resp.Data["metadata"]; meta {
			return inner, nil
		}
	}
	return resp.Data, nil
}

// httpError is a non-2xx answer from Vault.
type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return fmt.Sprintf("%d %s", e.status, e.msg) }

// do performs one API call against path (under /v1/), decoding the JSON
// answer into out when out is not nil. A non-2xx status is an *httpError
// carrying Vault's error messages.
func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Request", "true")
	if t := c.currentToken(); t != "" {
		req.Header.Set("X-Vault-Token", t)
	}
	if c.cfg.Namespace != "" {
		req.Header.Set("X-Vault-Namespace", c.cfg.Namespace)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &httpError{status: resp.StatusCode, msg: errorMessage(resp.StatusCode, data)}
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// errorMessage joins the errors of a Vault error body, falling back to the
// status text.
func errorMessage(status int, body []byte) string {
	var e struct {
		Errors []string `json:"errors"`
	}
	if json.Unmarshal(body, &e) == nil && len(e.Errors) > 0 {
		return strings.Join(e.Errors, "; ")
	}
	return strings.ToLower(http.StatusText(status))
}

// Lookup reads the secret ref names and returns the value of its key, which
// must be a string.
func Lookup(ctx context.Context, r Reader, ref config.Ref) (string, error) {
	data, err := r.Read(ctx, ref.Path)
	if err != nil {
		return "", err
	}
	return pick(data, ref)
}

// pick returns ref.Key's string value from data.
func pick(data map[string]any, ref config.Ref) (string, error) {
	v, ok := data[ref.Key]
	if !ok {
		return "", fmt.Errorf("vault: %s has no key %q", ref.Path, ref.Key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("vault: %s key %q is not a string", ref.Path, ref.Key)
	}
	return s, nil
}

// Resolver resolves ${vault:...} references through a Reader, reading each
// path once per Resolver.
type Resolver struct {
	Reader Reader
	cache  map[string]map[string]any
}

// Resolve replaces every ${vault:PATH#KEY} in s with the secret's value.
func (rv *Resolver) Resolve(ctx context.Context, s string) (string, error) {
	return config.ExpandVault(s, func(ref config.Ref) (string, error) {
		data, ok := rv.cache[ref.Path]
		if !ok {
			var err error
			if data, err = rv.Reader.Read(ctx, ref.Path); err != nil {
				return "", err
			}
			if rv.cache == nil {
				rv.cache = map[string]map[string]any{}
			}
			rv.cache[ref.Path] = data
		}
		return pick(data, ref)
	})
}

// ResolveMap returns a copy of m with every value resolved. Values without
// references are copied as they are.
func (rv *Resolver) ResolveMap(ctx context.Context, m map[string]string) (map[string]string, error) {
	if m == nil {
		return nil, nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		r, err := rv.Resolve(ctx, v)
		if err != nil {
			return nil, err
		}
		out[k] = r
	}
	return out, nil
}

// Resolve resolves the references in s, reading each path once.
func Resolve(ctx context.Context, r Reader, s string) (string, error) {
	return (&Resolver{Reader: r}).Resolve(ctx, s)
}

// ResolveMap resolves the references in every value of m, reading each path
// once.
func ResolveMap(ctx context.Context, r Reader, m map[string]string) (map[string]string, error) {
	return (&Resolver{Reader: r}).ResolveMap(ctx, m)
}
