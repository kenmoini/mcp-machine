package command

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kenmoini/mcp-machine/internal/config"
)

func TestResolveMovesVaultRefsToSecretEnv(t *testing.T) {
	old := lookupEnv
	lookupEnv = func(name string) (string, bool) { return "env-" + name, name == "PLAIN" }
	defer func() { lookupEnv = old }()

	cfg := testConfig()
	spec := config.ServerSpec{
		Name:    "a",
		Command: &config.CommandSpec{Command: "npx", Args: []string{"-y", "x"}, Env: map[string]string{"C1": "${vault:kv/data/a#c}", "C2": "plain"}},
		Env:     map[string]string{"S1": "pre-${vault:kv/data/a#s}", "S2": "${PLAIN}"},
		Container: &config.ContainerOverrides{
			Env: map[string]string{"O1": "${vault:kv/data/b#o}", "C2": "${vault:kv/data/b#override}"},
		},
	}
	res, err := Resolve(cfg, spec)
	if err != nil {
		t.Fatal(err)
	}
	cs := res.Container
	wantEnv := map[string]string{"S2": "${PLAIN}"}
	if !reflect.DeepEqual(cs.Env, wantEnv) {
		t.Errorf("Env = %v, want %v", cs.Env, wantEnv)
	}
	wantSecret := map[string]string{
		"C1": "${vault:kv/data/a#c}",
		"C2": "${vault:kv/data/b#override}",
		"S1": "pre-${vault:kv/data/a#s}",
		"O1": "${vault:kv/data/b#o}",
	}
	if !reflect.DeepEqual(cs.SecretEnv, wantSecret) {
		t.Errorf("SecretEnv = %v, want %v", cs.SecretEnv, wantSecret)
	}

	// Without references nothing changes and SecretEnv stays nil.
	res, err = Resolve(cfg, config.ServerSpec{Name: "b", Command: &config.CommandSpec{Command: "npx"}, Env: map[string]string{"A": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Container.SecretEnv != nil || res.Container.Env["A"] != "1" {
		t.Errorf("plain: %+v", res.Container)
	}
}

func TestResolveRunLineVaultRefs(t *testing.T) {
	old := lookupEnv
	lookupEnv = func(name string) (string, bool) { return "env-" + name, name == "PLAIN" }
	defer func() { lookupEnv = old }()
	cfg := testConfig()

	// In -e values (both forms, composed with ${VAR}): allowed, kept verbatim.
	res, err := Resolve(cfg, config.ServerSpec{Name: "a", Command: &config.CommandSpec{
		Run: `docker run -i --rm -e TOKEN=${vault:kv/data/a#tok} --env MIX=${PLAIN}:${vault:kv/data/a#tok} -e KEEP=${PLAIN} img --flag`,
	}})
	if err != nil {
		t.Fatal(err)
	}
	cs := res.Container
	if cs.Image != "img" || !reflect.DeepEqual(cs.Command, []string{"--flag"}) {
		t.Errorf("image/command = %q %v", cs.Image, cs.Command)
	}
	if !reflect.DeepEqual(cs.Env, map[string]string{"KEEP": "env-PLAIN"}) {
		t.Errorf("Env = %v", cs.Env)
	}
	wantSecret := map[string]string{"TOKEN": "${vault:kv/data/a#tok}", "MIX": "env-PLAIN:${vault:kv/data/a#tok}"}
	if !reflect.DeepEqual(cs.SecretEnv, wantSecret) {
		t.Errorf("SecretEnv = %v, want %v", cs.SecretEnv, wantSecret)
	}

	// Anywhere else: refused.
	for _, tc := range []struct{ run, want string }{
		{"docker run -i img --token ${vault:kv/data/a#k}", "run line's arguments"},
		{"docker run -i ${vault:kv/data/a#k}", "run line's image"},
		{"docker run -i -v ${vault:kv/data/a#k}:/x img", "only allowed in the run line's -e values"},
		{"docker run -i -w ${vault:kv/data/a#k} img", "only allowed in the run line's -e values"},
		{"docker run -i -p ${vault:kv/data/a#k}:80 img", "only allowed in the run line's -e values"},
	} {
		_, err := Resolve(cfg, config.ServerSpec{Name: "a", Command: &config.CommandSpec{Run: tc.run}})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err = %v, want containing %q", tc.run, err, tc.want)
		}
	}

	// command.args: refused too.
	_, err = Resolve(cfg, config.ServerSpec{Name: "a", Command: &config.CommandSpec{Command: "npx", Args: []string{"${vault:kv/data/a#k}"}}})
	if err == nil || !strings.Contains(err.Error(), "not allowed in command.args") {
		t.Errorf("args: %v", err)
	}
}
