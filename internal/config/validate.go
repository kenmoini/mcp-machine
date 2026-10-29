package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// NameRE is the DNS-label-style pattern every server name must match.
var NameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// reservedNames are server names that collide with route words.
var reservedNames = map[string]bool{"new": true, "import": true}

// minStopTimeout is the smallest allowed podman.stop_timeout.
const minStopTimeout = time.Second

// Validate checks c and returns nil when it is valid. Otherwise it returns
// every problem found, joined with errors.Join. Problems scoped to a server
// start with `server "<name>":`.
func Validate(c *Config) error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
		add("tls.cert_file and tls.key_file must both be set")
	}
	if !strings.HasPrefix(c.Podman.Socket, "unix://") && !strings.HasPrefix(c.Podman.Socket, "tcp://") {
		add("podman.socket must start with unix:// or tcp://")
	}
	if c.Podman.StopTimeout < minStopTimeout {
		add("podman.stop_timeout must be at least 1s")
	}
	if c.PublicURL != "" && !isAbsoluteURL(c.PublicURL) {
		add("public_url must be an absolute URL")
	}
	for i, o := range c.AllowedOrigins {
		if !isAbsoluteURL(o) {
			add("allowed_origins[%d] must be an absolute origin like http://host:port", i)
		}
	}

	errs = append(errs, validateVault(c.Vault)...)

	seen := make(map[string]bool, len(c.Servers))
	for i := range c.Servers {
		s := &c.Servers[i]
		if seen[s.Name] {
			add("duplicate server name %q", s.Name)
		}
		seen[s.Name] = true
		errs = append(errs, validateServer(s, c.Runtimes)...)
	}

	return errors.Join(errs...)
}

// validateServer returns the problems found in one server entry.
func validateServer(s *ServerSpec, runtimes map[string]string) []error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("server %q: %s", s.Name, fmt.Sprintf(format, args...)))
	}

	switch {
	case !NameRE.MatchString(s.Name):
		add("invalid name (must match %s)", NameRE)
	case reservedNames[s.Name]:
		add("name is reserved")
	}

	switch s.Type {
	case "remote", "command", "build":
		present := map[string]bool{
			"remote":  s.Remote != nil,
			"command": s.Command != nil,
			"build":   s.Build != nil,
		}
		if !present[s.Type] {
			add("%s section required", s.Type)
		}
		for name, ok := range present {
			if ok && name != s.Type {
				add("only the %s section may be set", s.Type)
				break
			}
		}
	default:
		add("type must be remote, command, or build")
	}

	if s.Remote != nil {
		switch s.Remote.Transport {
		case "http", "sse", "auto":
		default:
			add("remote.transport must be http, sse, or auto")
		}
	}
	if s.Command != nil {
		switch {
		case s.Command.Command != "" && s.Command.Run != "":
			add("command.command and command.run are mutually exclusive")
		case s.Command.Command == "" && s.Command.Run == "":
			add("one of command.command or command.run is required")
		case s.Command.Command != "":
			if _, ok := runtimes[s.Command.Command]; !ok {
				add("command.command %q is not a configured runtime", s.Command.Command)
			}
		}
	}
	if s.Build != nil && s.Build.Auth != nil && s.Build.Auth.KnownHostsFile != "" && s.Build.Auth.SSHKeyFile == "" {
		add("build.auth.known_hosts_file requires build.auth.ssh_key_file")
	}
	if s.Serve != nil {
		switch s.Serve.Mode {
		case "stdio":
		case "http":
			if s.Serve.Port <= 0 {
				add("serve.port required when serve.mode is http")
			}
		default:
			add("serve.mode must be stdio or http")
		}
	}
	return errs
}

// validateVault returns the problems found in the vault section. An empty
// section is valid; with an address, exactly one way to authenticate must be
// given.
func validateVault(v VaultConfig) []error {
	var errs []error
	add := func(msg string) {
		errs = append(errs, errors.New(msg))
	}
	hasAuth := v.Token != "" || v.TokenFile != "" || v.AppRole != nil
	if v.Address == "" {
		switch {
		case v.Token != "":
			add("vault.token needs vault.address")
		case v.TokenFile != "":
			add("vault.token_file needs vault.address")
		case v.AppRole != nil:
			add("vault.approle needs vault.address")
		}
		return errs
	}
	if u, err := url.Parse(v.Address); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		add("vault.address must be an absolute http(s) URL")
	}
	if v.Timeout < time.Second {
		add("vault.timeout must be at least 1s")
	}
	if v.TLSSkipVerify && v.CAFile != "" {
		add("vault.tls_skip_verify and vault.ca_file are mutually exclusive")
	}
	if v.Token != "" && v.TokenFile != "" {
		add("vault.token and vault.token_file are mutually exclusive")
	}
	if a := v.AppRole; a != nil {
		switch {
		case v.Token != "":
			add("vault.approle and vault.token are mutually exclusive")
		case v.TokenFile != "":
			add("vault.approle and vault.token_file are mutually exclusive")
		}
		switch {
		case a.RoleID != "" && a.RoleIDFile != "":
			add("vault.approle.role_id and vault.approle.role_id_file are mutually exclusive")
		case a.RoleID == "" && a.RoleIDFile == "":
			add("vault.approle needs role_id or role_id_file")
		}
		switch {
		case a.SecretID != "" && a.SecretIDFile != "":
			add("vault.approle.secret_id and vault.approle.secret_id_file are mutually exclusive")
		case a.SecretID == "" && a.SecretIDFile == "":
			add("vault.approle needs secret_id or secret_id_file")
		}
	}
	if !hasAuth {
		add("vault needs token, token_file, or approle")
	}
	return errs
}

// isAbsoluteURL reports whether raw parses as a URL with a scheme and host.
func isAbsoluteURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme != "" && u.Host != ""
}
