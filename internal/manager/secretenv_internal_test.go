package manager

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kenmoini/mcp-machine/internal/runtime"
)

func TestWrapEntrypointComposesLikePodman(t *testing.T) {
	wrapper := []string{"/bin/sh", "-c", wrapperScript, "mcp-machine"}
	cases := []struct {
		name string
		spec runtime.ContainerSpec
		img  runtime.ImageConfig
		want []string
		err  string
	}{
		{"command set", runtime.ContainerSpec{Command: []string{"npx", "-y", "x"}},
			runtime.ImageConfig{Entrypoint: []string{"docker-entrypoint.sh"}, Cmd: []string{"node"}},
			[]string{"docker-entrypoint.sh", "npx", "-y", "x"}, ""},
		{"cmd only", runtime.ContainerSpec{},
			runtime.ImageConfig{Entrypoint: []string{"/app/server"}, Cmd: []string{"--port", "3000"}},
			[]string{"/app/server", "--port", "3000"}, ""},
		{"entrypoint only", runtime.ContainerSpec{},
			runtime.ImageConfig{Entrypoint: []string{"/app/server"}},
			[]string{"/app/server"}, ""},
		{"no entrypoint", runtime.ContainerSpec{Command: []string{"node", "x.js"}},
			runtime.ImageConfig{Cmd: []string{"node"}},
			[]string{"node", "x.js"}, ""},
		{"spec entrypoint wins", runtime.ContainerSpec{Entrypoint: []string{"/custom"}, Command: []string{"a"}},
			runtime.ImageConfig{Entrypoint: []string{"/image"}, Cmd: []string{"b"}},
			[]string{"/custom", "a"}, ""},
		{"nothing to run", runtime.ContainerSpec{Image: "img"}, runtime.ImageConfig{}, nil, "declares no entrypoint or command"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := tc.spec
			err := wrapEntrypoint(&spec, tc.img)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(spec.Entrypoint, wrapper) {
				t.Errorf("entrypoint = %q", spec.Entrypoint)
			}
			if !reflect.DeepEqual(spec.Command, tc.want) {
				t.Errorf("command = %q, want %q", spec.Command, tc.want)
			}
		})
	}
}

func TestWrapperScriptShape(t *testing.T) {
	if strings.Contains(wrapperScript, "'") {
		t.Error("the wrapper holds a single quote; it is passed as one sh -c argument")
	}
	for _, want := range []string{"set -a", ". \"$f\"", "set +a", "rm -f \"$f\"", `exec "$@"`, deliveryPath, "sleep 0.1 2>/dev/null || sleep 1"} {
		if !strings.Contains(wrapperScript, want) {
			t.Errorf("wrapper lacks %q", want)
		}
	}
	if !strings.HasSuffix(deliverCmd[2], deliveryPath) || !strings.HasPrefix(deliverCmd[2], "umask 077 && cat > ") {
		t.Errorf("deliver command = %q", deliverCmd)
	}
}

func TestEnvFileQuoting(t *testing.T) {
	got, err := envFile(map[string]string{
		"Z":       "plain",
		"A":       "it's",
		"NL":      "two\nlines",
		"DOLLAR":  "$HOME `id` \\ \"q\"",
		"EMPTY":   "",
		"_under1": "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "A='it'\\''s'\n" +
		"DOLLAR='$HOME `id` \\ \"q\"'\n" +
		"EMPTY=''\n" +
		"NL='two\nlines'\n" +
		"Z='plain'\n" +
		"_under1='x'\n"
	if string(got) != want {
		t.Errorf("env file =\n%q\nwant\n%q", got, want)
	}
	for _, bad := range []string{"1X", "A-B", "A B", "", "A;rm"} {
		if _, err := envFile(map[string]string{bad: "v"}); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
	if b, err := envFile(nil); err != nil || len(b) != 0 {
		t.Errorf("nil map: %q %v", b, err)
	}
}

func TestSecretMountOptions(t *testing.T) {
	m := secretMount()
	if m.Type != "tmpfs" || m.Target != deliveryDir || m.ReadOnly {
		t.Errorf("mount = %+v", m)
	}
	for _, opt := range []string{"nosuid", "nodev", "noexec", "mode=1777", "size=1m"} {
		if !strings.Contains(m.Options, opt) {
			t.Errorf("options %q lack %s", m.Options, opt)
		}
	}
}
