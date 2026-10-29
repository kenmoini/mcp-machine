package config_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/kenmoini/mcp-machine/internal/config"
)

func TestParseRefs(t *testing.T) {
	ref := func(p, k string) config.Ref { return config.Ref{Path: p, Key: k} }
	cases := []struct {
		name string
		in   string
		want []config.Ref
		err  string
	}{
		{"none", "plain ${VAR} text", nil, ""},
		{"single", "${vault:kv/data/github#token}", []config.Ref{ref("kv/data/github", "token")}, ""},
		{"composed", "Bearer ${vault:kv/data/github#token}", []config.Ref{ref("kv/data/github", "token")}, ""},
		{"mixed", "${USER}:${vault:kv/data/db#password}", []config.Ref{ref("kv/data/db", "password")}, ""},
		{"kv v1", "${vault:kv/github#token}", []config.Ref{ref("kv/github", "token")}, ""},
		{"two", "${vault:kv/data/a#x}/${vault:kv/data/b#y}", []config.Ref{ref("kv/data/a", "x"), ref("kv/data/b", "y")}, ""},
		{"key with dots and dashes", "${vault:kv/data/a#api-key.v2}", []config.Ref{ref("kv/data/a", "api-key.v2")}, ""},
		{"no key", "${vault:kv/data/a}", nil, `invalid reference "${vault:kv/data/a}"`},
		{"empty key", "${vault:kv/data/a#}", nil, `invalid reference "${vault:kv/data/a#}"`},
		{"empty path", "${vault:#token}", nil, `invalid reference "${vault:#token}"`},
		{"leading slash", "${vault:/kv/data/a#token}", nil, `invalid reference "${vault:/kv/data/a#token}"`},
		{"dot dot", "${vault:kv/../sys#token}", nil, `invalid reference "${vault:kv/../sys#token}"`},
		{"space", "${vault:kv/data/a b#token}", nil, `invalid reference "${vault:kv/data/a b#token}"`},
		{"empty", "${vault:}", nil, `invalid reference "${vault:}"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := config.ParseRefs(tc.in)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want containing %q", err, tc.err)
				}
				if !errors.Is(err, config.ErrBadRef) {
					t.Errorf("err does not wrap ErrBadRef: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRefs: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("refs = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestHasVaultRef(t *testing.T) {
	for in, want := range map[string]bool{
		"":                     false,
		"${VAR}":               false,
		"${vault:kv/data/a#b}": true,
		"x${vault:kv/a}y":      true, // malformed still counts: the loader rejects it
		"vault:kv/data/a#b":    false,
		"$vault:kv/data/a#b":   false,
	} {
		if got := config.HasVaultRef(in); got != want {
			t.Errorf("HasVaultRef(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestExpandVault(t *testing.T) {
	resolve := func(r config.Ref) (string, error) {
		switch r {
		case config.Ref{Path: "kv/data/a", Key: "x"}:
			return "AX", nil
		case config.Ref{Path: "kv/data/b", Key: "y"}:
			return "BY", nil
		}
		return "", errors.New("boom")
	}
	got, err := config.ExpandVault("pre ${vault:kv/data/a#x} ${VAR} ${vault:kv/data/b#y} post", resolve)
	if err != nil {
		t.Fatal(err)
	}
	if want := "pre AX ${VAR} BY post"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	_, err = config.ExpandVault("${vault:kv/data/zzz#k}", resolve)
	if err == nil || !strings.Contains(err.Error(), "${vault:kv/data/zzz#k}") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want one naming the reference and the cause", err)
	}

	_, err = config.ExpandVault("${vault:bad}", resolve)
	if !errors.Is(err, config.ErrBadRef) {
		t.Errorf("malformed: err = %v, want ErrBadRef", err)
	}

	// No refs: the string is returned unchanged without calling resolve.
	got, err = config.ExpandVault("plain ${VAR}", func(config.Ref) (string, error) { t.Fatal("resolve called"); return "", nil })
	if err != nil || got != "plain ${VAR}" {
		t.Errorf("plain: %q, %v", got, err)
	}
}

func TestMaskVaultRefsRoundTrip(t *testing.T) {
	in := `docker run -i -e TOKEN=${vault:kv/data/a#tok} -e PLAIN=${VAR} img --flag ${vault:kv/data/b#arg}`
	masked, refs := config.MaskVaultRefs(in)
	if config.HasVaultRef(masked) {
		t.Fatalf("masked still holds a ref: %q", masked)
	}
	if len(refs) != 2 {
		t.Fatalf("refs = %+v", refs)
	}
	if !strings.Contains(masked, "${VAR}") {
		t.Errorf("plain reference was touched: %q", masked)
	}
	if got := config.UnmaskVaultRefs(masked, refs); got != in {
		t.Errorf("round trip = %q, want %q", got, in)
	}
	// A mask token is a plain ${NAME}, so parsers that keep ${NAME} verbatim
	// carry it through.
	for _, tok := range config.VaultMasks(refs) {
		if !strings.HasPrefix(tok, "${") || !strings.HasSuffix(tok, "}") || strings.ContainsAny(tok[2:len(tok)-1], ":#/- ") {
			t.Errorf("mask %q is not a plain ${NAME}", tok)
		}
	}
	if got := config.UnmaskVaultRefs("nothing", nil); got != "nothing" {
		t.Errorf("unmask without refs = %q", got)
	}
}

func TestCheckVaultRefsPerField(t *testing.T) {
	const ref = "${vault:kv/data/a#k}"
	allowed := []struct {
		name string
		spec config.ServerSpec
	}{
		{"env", config.ServerSpec{Name: "s", Env: map[string]string{"A": ref}}},
		{"command.env", config.ServerSpec{Name: "s", Command: &config.CommandSpec{Command: "npx", Env: map[string]string{"A": ref}}}},
		{"container.env", config.ServerSpec{Name: "s", Container: &config.ContainerOverrides{Env: map[string]string{"A": "x" + ref}}}},
		{"remote.headers", config.ServerSpec{Name: "s", Remote: &config.RemoteSpec{URL: "https://x", Headers: map[string]string{"Authorization": "Bearer " + ref}}}},
		{"build.auth.token", config.ServerSpec{Name: "s", Build: &config.BuildSpec{Repo: "https://x", Auth: &config.GitAuth{Token: ref}}}},
		{"run -e value", config.ServerSpec{Name: "s", Command: &config.CommandSpec{Run: "docker run -i -e TOKEN=" + ref + " img"}}},
		{"run -e attached", config.ServerSpec{Name: "s", Command: &config.CommandSpec{Run: "docker run -i -eTOKEN=" + ref + " img"}}},
		{"run --env", config.ServerSpec{Name: "s", Command: &config.CommandSpec{Run: "docker run --env TOKEN=" + ref + " img"}}},
	}
	for _, tc := range allowed {
		t.Run("allowed/"+tc.name, func(t *testing.T) {
			refs, errs := config.CheckVaultRefs(tc.spec)
			if len(errs) != 0 {
				t.Fatalf("errs = %v", errs)
			}
			if len(refs) != 1 || refs[0] != (config.Ref{Path: "kv/data/a", Key: "k"}) {
				t.Errorf("refs = %+v", refs)
			}
		})
	}

	refused := []struct {
		name  string
		spec  config.ServerSpec
		field string
	}{
		{"command.args", config.ServerSpec{Name: "s", Command: &config.CommandSpec{Command: "npx", Args: []string{"--token", ref}}}, "command.args"},
		{"run image args", config.ServerSpec{Name: "s", Command: &config.CommandSpec{Run: "docker run -i img --token " + ref}}, "command.run"},
		{"run image", config.ServerSpec{Name: "s", Command: &config.CommandSpec{Run: "docker run -i " + ref}}, "command.run"},
		{"run volume", config.ServerSpec{Name: "s", Command: &config.CommandSpec{Run: "docker run -i -v " + ref + ":/x img"}}, "command.run"},
		{"build.args", config.ServerSpec{Name: "s", Build: &config.BuildSpec{Repo: "https://x", Args: map[string]string{"T": ref}}}, "build.args"},
		{"build.repo", config.ServerSpec{Name: "s", Build: &config.BuildSpec{Repo: "https://" + ref + "@x"}}, "build.repo"},
		{"remote.url", config.ServerSpec{Name: "s", Remote: &config.RemoteSpec{URL: "https://x/" + ref}}, "remote.url"},
		{"container.image", config.ServerSpec{Name: "s", Container: &config.ContainerOverrides{Image: ref}}, "container.image"},
		{"container.volumes", config.ServerSpec{Name: "s", Container: &config.ContainerOverrides{Volumes: []string{ref + ":/x"}}}, "container.volumes"},
	}
	for _, tc := range refused {
		t.Run("refused/"+tc.name, func(t *testing.T) {
			_, errs := config.CheckVaultRefs(tc.spec)
			if len(errs) != 1 {
				t.Fatalf("errs = %v, want one", errs)
			}
			want := `server "s": ${vault:...} is not allowed in ` + tc.field
			if !strings.Contains(errs[0].Error(), want) {
				t.Errorf("err = %q, want containing %q", errs[0], want)
			}
		})
	}

	// A malformed reference in an allowed field is reported with the field.
	_, errs := config.CheckVaultRefs(config.ServerSpec{Name: "s", Env: map[string]string{"A": "${vault:nokey}"}})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), `server "s": env: invalid reference "${vault:nokey}"`) {
		t.Errorf("malformed: errs = %v", errs)
	}

	// Two refs in different fields are both returned, in a stable order;
	// the same ref twice is returned once.
	refs, errs := config.CheckVaultRefs(config.ServerSpec{
		Name:    "s",
		Env:     map[string]string{"B": "${vault:kv/data/b#k}", "A": "${vault:kv/data/a#k}"},
		Command: &config.CommandSpec{Command: "npx", Env: map[string]string{"C": "${vault:kv/data/a#k}"}},
	})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	want := []config.Ref{{Path: "kv/data/a", Key: "k"}, {Path: "kv/data/b", Key: "k"}}
	if !reflect.DeepEqual(refs, want) {
		t.Errorf("refs = %+v, want %+v", refs, want)
	}

	// A run line that does not parse is left to the resolver.
	_, errs = config.CheckVaultRefs(config.ServerSpec{Name: "s", Command: &config.CommandSpec{Run: "docker run -i img $(bad)"}})
	if len(errs) != 0 {
		t.Errorf("unparsable run: errs = %v", errs)
	}
}
