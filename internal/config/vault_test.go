package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kenmoini/mcp-machine/internal/config"
)

const vaultYAML = `
vault:
  address: https://vault.example:8200
  token: "${VAULT_TOKEN}"
servers:
  - name: gh
    type: command
    command:
      command: npx
      args: ["-y", "server"]
      run: ""
    env:
      GITHUB_TOKEN: "${vault:kv/data/github#token}"
      MIXED: "${PLAIN}-${vault:kv/data/github#token}"
  - name: cp
    type: remote
    remote:
      url: https://x.example/mcp
      headers:
        Authorization: "Bearer ${vault:kv/data/github#token}"
  - name: b
    type: build
    build:
      repo: https://example.com/r.git
      auth:
        token: "${vault:kv/data/git#token}"
`

func TestParseKeepsVaultRefsVerbatim(t *testing.T) {
	lookup := func(k string) (string, bool) { return "v-" + k, true }
	c, err := config.Parse([]byte(vaultYAML), lookup)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Vault.Address != "https://vault.example:8200" || c.Vault.Token != "v-VAULT_TOKEN" {
		t.Errorf("vault section = %+v", c.Vault)
	}
	if c.Vault.Timeout != 10*time.Second {
		t.Errorf("Timeout = %v, want 10s default", c.Vault.Timeout)
	}
	if got := c.Servers[0].Env["GITHUB_TOKEN"]; got != "${vault:kv/data/github#token}" {
		t.Errorf("env ref = %q, want verbatim", got)
	}
	if got := c.Servers[0].Env["MIXED"]; got != "v-PLAIN-${vault:kv/data/github#token}" {
		t.Errorf("mixed = %q: ${VAR} expanded, ref kept", got)
	}
	if got := c.Servers[1].Remote.Headers["Authorization"]; got != "Bearer ${vault:kv/data/github#token}" {
		t.Errorf("header = %q", got)
	}
	if got := c.Servers[2].Build.Auth.Token; got != "${vault:kv/data/git#token}" {
		t.Errorf("token = %q", got)
	}
}

func TestParseVaultRefNeedsAddress(t *testing.T) {
	y := strings.Replace(vaultYAML, "  address: https://vault.example:8200\n", "", 1)
	_, err := config.Parse([]byte(y), anyEnv)
	if err == nil {
		t.Fatal("expected error")
	}
	want := `config: ${vault:...} references need vault.address (referenced by servers "gh", "cp", "b")`
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}

	// Global references (registries) name no server.
	_, err = config.Parse([]byte("registries:\n  - registry: r\n    password: \"${vault:kv/data/r#p}\"\n"), anyEnv)
	if err == nil || err.Error() != "config: ${vault:...} references need vault.address" {
		t.Errorf("registries: error = %v", err)
	}
}

func TestParseRefusesVaultRefOutsideAllowedFields(t *testing.T) {
	for _, tc := range []struct{ name, yaml, want string }{
		{"args", `
servers:
  - {name: a, type: command, command: {command: npx, args: ["${vault:kv/data/a#k}"]}}
`, `config: server "a": ${vault:...} is not allowed in command.args`},
		{"build args", `
servers:
  - {name: a, type: build, build: {repo: "https://x/r.git", args: {T: "${vault:kv/data/a#k}"}}}
`, `config: server "a": ${vault:...} is not allowed in build.args`},
		{"run image args", `
servers:
  - {name: a, type: command, command: {run: "docker run -i img --token ${vault:kv/data/a#k}"}}
`, `config: server "a": ${vault:...} is not allowed in command.run`},
		{"malformed", `
servers:
  - {name: a, type: command, command: {command: npx}, env: {A: "${vault:kv/data/a}"}}
`, `config: server "a": env: invalid reference "${vault:kv/data/a}"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Parse([]byte("vault: {address: https://v:8200}\n"+tc.yaml), anyEnv)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestParseAllowsVaultRefInRunEnvValue(t *testing.T) {
	y := `
vault: {address: https://v:8200}
servers:
  - {name: a, type: command, command: {run: "docker run -i -e TOKEN=${vault:kv/data/a#k} img"}}
`
	c, err := config.Parse([]byte(y), anyEnv)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := c.Servers[0].Command.Run; !strings.Contains(got, "${vault:kv/data/a#k}") {
		t.Errorf("run = %q, want the reference kept", got)
	}
}

func TestParseVaultRefErrorsJoinWithUnsetVars(t *testing.T) {
	y := `
servers:
  - {name: a, type: command, command: {command: npx}, env: {A: "${vault:kv/data/a#k}", B: "${NOPE}"}}
`
	_, err := config.Parse([]byte(y), noEnv)
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"${NOPE} is not set", "need vault.address"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want containing %q", err, want)
		}
	}
}

func TestDropInRejectsVaultKey(t *testing.T) {
	l := layoutDir(t, map[string]string{
		"config.yaml":     remoteYAML("a", ""),
		"config.d/v.yaml": "vault:\n  address: https://v:8200\n",
	})
	_, err := l.Load()
	if err == nil || !strings.Contains(err.Error(), "vault") {
		t.Errorf("Load: %v, want an error naming the vault key", err)
	}
}

func TestLoadEnvOverridesVault(t *testing.T) {
	t.Setenv("VAULT_ADDR", "https://env.example:8200")
	t.Setenv("VAULT_TOKEN", "hvs.env")
	t.Setenv("VAULT_NAMESPACE", "ns")
	t.Setenv("VAULT_CACERT", "/ca.pem")
	t.Setenv("VAULT_SKIP_VERIFY", "true")
	y := "vault:\n  address: https://file.example:8200\n  token: file-token\n" +
		"servers:\n  - {name: a, type: command, command: {command: npx}, env: {A: \"${vault:kv/data/a#k}\"}}\n"
	l := layoutDir(t, map[string]string{"config.yaml": y})
	for _, load := range []struct {
		name string
		fn   func() (*config.Set, error)
	}{{"Load", l.Load}, {"LoadRaw", l.LoadRaw}} {
		t.Run(load.name, func(t *testing.T) {
			set, err := load.fn()
			if err != nil {
				t.Fatalf("%s: %v", load.name, err)
			}
			v := set.Config.Vault
			if v.Address != "https://env.example:8200" || v.Token != "hvs.env" || v.Namespace != "ns" || v.CAFile != "/ca.pem" || !v.TLSSkipVerify {
				t.Errorf("vault = %+v, want env overrides", v)
			}
		})
	}

	// With no address in the file, VAULT_ADDR alone satisfies the references.
	y = strings.Replace(y, "  address: https://file.example:8200\n", "", 1)
	l = layoutDir(t, map[string]string{"config.yaml": y})
	if _, err := l.Load(); err != nil {
		t.Errorf("Load with VAULT_ADDR only: %v", err)
	}
	if _, err := l.LoadRaw(); err != nil {
		t.Errorf("LoadRaw with VAULT_ADDR only: %v", err)
	}

	// An empty VAULT_SKIP_VERIFY does not override; "0" and "false" clear it.
	t.Setenv("VAULT_SKIP_VERIFY", "")
	y = "vault:\n  address: https://file.example:8200\n  tls_skip_verify: true\n"
	l = layoutDir(t, map[string]string{"config.yaml": y})
	if set, err := l.Load(); err != nil || !set.Config.Vault.TLSSkipVerify {
		t.Errorf("empty VAULT_SKIP_VERIFY: %+v %v", set, err)
	}
	t.Setenv("VAULT_SKIP_VERIFY", "false")
	if set, err := l.Load(); err != nil || set.Config.Vault.TLSSkipVerify {
		t.Errorf("VAULT_SKIP_VERIFY=false: %+v %v", set, err)
	}
}

func TestLoadVaultTokenNeverInRawExpansion(t *testing.T) {
	// LoadRaw keeps ${VAULT_TOKEN} as written in the file, like any ${VAR}.
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("vault:\n  address: https://v:8200\n  token: \"${VAULT_TOKEN}\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	set, err := config.Layout{BaseFile: path}.LoadRaw()
	if err != nil {
		t.Fatal(err)
	}
	if set.Config.Vault.Token != "${VAULT_TOKEN}" {
		t.Errorf("raw token = %q", set.Config.Vault.Token)
	}
}

func TestValidateVault(t *testing.T) {
	cases := []struct{ name, yaml, want string }{
		{"valid token", "vault: {address: https://v:8200, token: t}\n", ""},
		{"valid approle", "vault: {address: https://v:8200, approle: {role_id: r, secret_id_file: /s}}\n", ""},
		{"valid token_file", "vault: {address: https://v:8200, token_file: /t}\n", ""},
		{"unset", "", ""},
		{"relative address", "vault: {address: vault:8200}\n", "vault.address must be an absolute http(s) URL"},
		{"ftp address", "vault: {address: ftp://v}\n", "vault.address must be an absolute http(s) URL"},
		{"timeout", "vault: {address: https://v:8200, timeout: 10ms}\n", "vault.timeout must be at least 1s"},
		{"token and file", "vault: {address: https://v:8200, token: t, token_file: /t}\n", "vault.token and vault.token_file are mutually exclusive"},
		{"token and approle", "vault: {address: https://v:8200, token: t, approle: {role_id: r, secret_id: s}}\n", "vault.approle and vault.token are mutually exclusive"},
		{"role ids", "vault: {address: https://v:8200, approle: {role_id: r, role_id_file: /r, secret_id: s}}\n", "vault.approle.role_id and vault.approle.role_id_file are mutually exclusive"},
		{"secret ids", "vault: {address: https://v:8200, approle: {role_id: r, secret_id: s, secret_id_file: /s}}\n", "vault.approle.secret_id and vault.approle.secret_id_file are mutually exclusive"},
		{"approle no role", "vault: {address: https://v:8200, approle: {secret_id: s}}\n", "vault.approle needs role_id or role_id_file"},
		{"approle no secret", "vault: {address: https://v:8200, approle: {role_id: r}}\n", "vault.approle needs secret_id or secret_id_file"},
		{"no auth", "vault: {address: https://v:8200}\n", "vault needs token, token_file, or approle"},
		{"skip verify with ca", "vault: {address: https://v:8200, token: t, ca_file: /ca, tls_skip_verify: true}\n", "vault.tls_skip_verify and vault.ca_file are mutually exclusive"},
		{"auth without address", "vault: {token: t}\n", "vault.token needs vault.address"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := config.Parse([]byte(tc.yaml), anyEnv)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			err = config.Validate(c)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("Validate: %v, want nil", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("Validate: %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestVaultConfigured(t *testing.T) {
	if (config.VaultConfig{}).Configured() {
		t.Error("empty is configured")
	}
	if !(config.VaultConfig{Address: "https://v"}).Configured() {
		t.Error("address is not configured")
	}
}

func TestHashUnchangedByVaultSupport(t *testing.T) {
	// A spec without references hashes exactly as before this feature: the
	// golden in TestHashIsStable covers that. A spec with a reference hashes
	// the reference text, so the hash does not depend on the secret's value
	// and differs between references.
	a := hashSpec()
	a.Env["T"] = "${vault:kv/data/a#k}"
	b := hashSpec()
	b.Env["T"] = "${vault:kv/data/b#k}"
	if config.Hash(a, "img") == config.Hash(b, "img") {
		t.Error("different references hash the same")
	}
	if config.Hash(a, "img") == config.Hash(hashSpec(), "img") {
		t.Error("a reference does not change the hash")
	}
	if got := config.Hash(a, "img"); got != config.Hash(a, "img") {
		t.Errorf("unstable: %s", got)
	}
}
