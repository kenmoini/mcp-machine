package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/kenmoini/mcp-machine/internal/sources/dockerrun"
)

// Ref is a reference to one key of a secret held by Vault, written in the
// configuration as ${vault:PATH#KEY}. Path is the API path under /v1/, for
// example kv/data/github for a KV v2 mount or kv/github for KV v1; Key is a
// key of the secret's data.
type Ref struct{ Path, Key string }

// String returns the reference as written: ${vault:PATH#KEY}.
func (r Ref) String() string { return "${vault:" + r.Path + "#" + r.Key + "}" }

// ErrBadRef is wrapped by the error for a ${vault:...} that is not
// ${vault:PATH#KEY}.
var ErrBadRef = errors.New("invalid ${vault:...} reference")

// badRef is the error for a malformed reference; errors.Is(err, ErrBadRef)
// holds for it.
type badRef string

func (e badRef) Error() string {
	return fmt.Sprintf("invalid reference %q: want ${vault:PATH#KEY}", string(e))
}

func (badRef) Is(target error) bool { return target == ErrBadRef }

// vaultAny matches every ${vault:...}, well-formed or not; vaultRef captures
// the path and key of a well-formed one.
var (
	vaultAny = regexp.MustCompile(`\$\{vault:[^}]*\}`)
	vaultRef = regexp.MustCompile(`^\$\{vault:([^}#]+)#([^}]+)\}$`)
)

// HasVaultRef reports whether s holds any ${vault:...}, well-formed or not.
func HasVaultRef(s string) bool { return vaultAny.MatchString(s) }

// ParseRefs returns every ${vault:PATH#KEY} in s, in order, or an error
// (wrapping ErrBadRef) naming the first ${vault:...} that is not one.
func ParseRefs(s string) ([]Ref, error) {
	var refs []Ref
	for _, m := range vaultAny.FindAllString(s, -1) {
		r, err := parseRef(m)
		if err != nil {
			return nil, err
		}
		refs = append(refs, r)
	}
	return refs, nil
}

// parseRef parses one ${vault:...} match.
func parseRef(m string) (Ref, error) {
	bad := func() (Ref, error) { return Ref{}, badRef(m) }
	sub := vaultRef.FindStringSubmatch(m)
	if sub == nil {
		return bad()
	}
	path, key := sub[1], sub[2]
	if strings.HasPrefix(path, "/") || strings.ContainsAny(path+key, " \t\r\n") {
		return bad()
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return bad()
		}
	}
	return Ref{Path: path, Key: key}, nil
}

// ExpandVault replaces each ${vault:PATH#KEY} in s with resolve's value for
// it. ${VAR} references are left alone. A resolve error is returned wrapped
// with the reference it concerns; a malformed reference is an ErrBadRef.
func ExpandVault(s string, resolve func(Ref) (string, error)) (string, error) {
	if !HasVaultRef(s) {
		return s, nil
	}
	var firstErr error
	out := vaultAny.ReplaceAllStringFunc(s, func(m string) string {
		if firstErr != nil {
			return m
		}
		r, err := parseRef(m)
		if err != nil {
			firstErr = err
			return m
		}
		v, err := resolve(r)
		if err != nil {
			firstErr = fmt.Errorf("%s: %w", m, err)
			return m
		}
		return v
	})
	if firstErr != nil {
		return "", firstErr
	}
	return out, nil
}

// maskPrefix starts the ${NAME} placeholder MaskVaultRefs substitutes for a
// reference, so parsers that keep ${NAME} as written (the run-line parser)
// carry it through untouched.
const maskPrefix = "__mcpm_vault_"

// MaskVaultRefs replaces every ${vault:...} in s (well-formed or not) with a
// plain ${NAME} placeholder and returns the placeholders' references in
// order. UnmaskVaultRefs reverses it. A malformed reference is masked too and
// comes back unchanged, with an empty Path and the original text as Key.
func MaskVaultRefs(s string) (masked string, refs []Ref) {
	masked = vaultAny.ReplaceAllStringFunc(s, func(m string) string {
		r, err := parseRef(m)
		if err != nil {
			r = Ref{Key: m}
		}
		refs = append(refs, r)
		return "${" + maskPrefix + strconv.Itoa(len(refs)-1) + "}"
	})
	return masked, refs
}

// VaultMasks returns the placeholders MaskVaultRefs substitutes for refs.
func VaultMasks(refs []Ref) []string {
	out := make([]string, len(refs))
	for i := range refs {
		out[i] = "${" + maskPrefix + strconv.Itoa(i) + "}"
	}
	return out
}

// UnmaskVaultRefs restores the references MaskVaultRefs replaced in s.
func UnmaskVaultRefs(s string, refs []Ref) string {
	for i, tok := range VaultMasks(refs) {
		r := refs[i]
		text := r.String()
		if r.Path == "" {
			text = r.Key // a malformed reference, kept as written
		}
		s = strings.ReplaceAll(s, tok, text)
	}
	return s
}

// CheckVaultRefs returns the Vault references of s, sorted and deduplicated,
// and the problems with them: a reference in a field that cannot take one
// (`server "a": ${vault:...} is not allowed in command.args`) or a malformed
// reference (`server "a": env: invalid reference "${vault:x}": ...`).
//
// References are allowed in env, command.env, container.env, the -e values
// of command.run, remote.headers and build.auth.token. They are refused
// everywhere else: command.args and the rest of command.run (they would show
// in the process list), build.args (they would be baked into the image),
// and the URL, path, image and volume fields.
func CheckVaultRefs(s ServerSpec) ([]Ref, []error) {
	var refs []Ref
	var errs []error
	allow := func(field, v string) {
		rs, err := ParseRefs(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("server %q: %s: %w", s.Name, field, err))
			return
		}
		refs = append(refs, rs...)
	}
	allowMap := func(field string, m map[string]string) {
		for _, k := range slices.Sorted(keys(m)) {
			allow(field, m[k])
		}
	}
	refuse := func(field, v string) {
		if HasVaultRef(v) {
			errs = append(errs, fmt.Errorf("server %q: ${vault:...} is not allowed in %s", s.Name, field))
		}
	}
	refuseMap := func(field string, m map[string]string) {
		for _, k := range slices.Sorted(keys(m)) {
			refuse(field, m[k])
		}
	}

	allowMap("env", s.Env)
	if r := s.Remote; r != nil {
		refuse("remote.url", r.URL)
		allowMap("remote.headers", r.Headers)
	}
	if c := s.Command; c != nil {
		allowMap("command.env", c.Env)
		for _, a := range c.Args {
			refuse("command.args", a)
		}
		checkRunLine(c.Run, allow, refuse)
	}
	if o := s.Container; o != nil {
		refuse("container.image", o.Image)
		for _, v := range o.Volumes {
			refuse("container.volumes", v)
		}
		allowMap("container.env", o.Env)
	}
	if b := s.Build; b != nil {
		refuse("build.repo", b.Repo)
		refuse("build.ref", b.Ref)
		refuse("build.subdir", b.Subdir)
		refuse("build.containerfile", b.Containerfile)
		refuseMap("build.args", b.Args)
		if b.Auth != nil {
			allow("build.auth.token", b.Auth.Token)
			refuse("build.auth.ssh_key_file", b.Auth.SSHKeyFile)
			refuse("build.auth.known_hosts_file", b.Auth.KnownHostsFile)
		}
	}
	if s.Serve != nil {
		refuse("serve.path", s.Serve.Path)
	}

	slices.SortFunc(refs, func(a, b Ref) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		return strings.Compare(a.Key, b.Key)
	})
	return slices.Compact(refs), errs
}

// checkRunLine sorts the references of a command.run line into the -e
// values (allowed) and everything else (refused). A line that does not
// parse is left alone: the command resolver reports it.
func checkRunLine(run string, allow, refuse func(field, v string)) {
	if !HasVaultRef(run) {
		return
	}
	masked, refs := MaskVaultRefs(run)
	parsed, err := dockerrun.Parse(masked)
	if err != nil {
		return
	}
	inEnv := map[string]bool{}
	for _, v := range parsed.Env {
		allow("command.run", UnmaskVaultRefs(v, refs))
		for _, tok := range VaultMasks(refs) {
			if strings.Contains(v, tok) {
				inEnv[tok] = true
			}
		}
	}
	for _, tok := range VaultMasks(refs) {
		if !inEnv[tok] {
			refuse("command.run", UnmaskVaultRefs(tok, refs))
		}
	}
}

func keys(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}
