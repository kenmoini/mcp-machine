package dockerrun_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kenmoini/mcp-machine/internal/runtime"
	"github.com/kenmoini/mcp-machine/internal/sources/dockerrun"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		want    dockerrun.Spec
		wantErr string
	}{
		{
			name: "full line",
			line: `docker run -i --rm -e A=1 --env B=2 -v /h:/c:ro -p 8080:80 --name x --network n -w /app img arg1 "arg 2"`,
			want: dockerrun.Spec{
				Image:       "img",
				Args:        []string{"arg1", "arg 2"},
				Env:         map[string]string{"A": "1", "B": "2"},
				Volumes:     []string{"/h:/c:ro"},
				Ports:       []string{"8080:80"},
				Name:        "x",
				Network:     "n",
				WorkDir:     "/app",
				Interactive: true,
				Remove:      true,
			},
		},
		{
			name: "podman prefix with long forms",
			line: `podman run --interactive --volume /a:/b --publish 1:2 --workdir /w img`,
			want: dockerrun.Spec{
				Image:       "img",
				Volumes:     []string{"/a:/b"},
				Ports:       []string{"1:2"},
				WorkDir:     "/w",
				Interactive: true,
			},
		},
		{
			name: "passthrough env",
			line: `docker run -e KEY -e K2=v img`,
			want: dockerrun.Spec{Image: "img", Env: map[string]string{"K2": "v"}, PassEnv: []string{"KEY"}},
		},
		{
			name: "equals-joined long forms",
			line: `docker run --env=K=V --name=x --network=n --volume=/a:/b --publish=1:2 --workdir=/w img`,
			want: dockerrun.Spec{
				Image:   "img",
				Env:     map[string]string{"K": "V"},
				Name:    "x",
				Network: "n",
				Volumes: []string{"/a:/b"},
				Ports:   []string{"1:2"},
				WorkDir: "/w",
			},
		},
		{
			name: "env value containing equals",
			line: `docker run -e URL=http://h/?a=b img`,
			want: dockerrun.Spec{Image: "img", Env: map[string]string{"URL": "http://h/?a=b"}},
		},
		{
			name: "double dash terminator",
			line: `docker run --rm -- img --not-a-flag x`,
			want: dockerrun.Spec{Image: "img", Args: []string{"--not-a-flag", "x"}, Remove: true},
		},
		{
			name: "flags after image are args",
			line: `docker run img -i --rm`,
			want: dockerrun.Spec{Image: "img", Args: []string{"-i", "--rm"}},
		},
		{
			name: "placeholder braces preserved",
			line: `docker run -i -e TOKEN=${GITHUB_TOKEN} img`,
			want: dockerrun.Spec{Image: "img", Interactive: true, Env: map[string]string{"TOKEN": "${GITHUB_TOKEN}"}},
		},
		{
			name: "bare variable normalized to braces",
			line: `docker run -e TOKEN=$GITHUB_TOKEN img`,
			want: dockerrun.Spec{Image: "img", Env: map[string]string{"TOKEN": "${GITHUB_TOKEN}"}},
		},
		{
			name: "quoted multiple placeholders",
			line: `docker run -e "X=${A}-${B}" img`,
			want: dockerrun.Spec{Image: "img", Env: map[string]string{"X": "${A}-${B}"}},
		},
		{
			name: "placeholder in args and set process env ignored",
			line: `docker run img --token ${HOME}`,
			want: dockerrun.Spec{Image: "img", Args: []string{"--token", "${HOME}"}},
		},
		{
			name: "attached short flag values",
			line: `docker run -eK=V -v/h:/c -p80:80 -w/app img`,
			want: dockerrun.Spec{
				Image:   "img",
				Env:     map[string]string{"K": "V"},
				Volumes: []string{"/h:/c"},
				Ports:   []string{"80:80"},
				WorkDir: "/app",
			},
		},
		{
			name: "attached passthrough env",
			line: `docker run -eKEY img`,
			want: dockerrun.Spec{Image: "img", PassEnv: []string{"KEY"}},
		},
		{name: "short cluster stays unknown", line: `docker run -id img`, wantErr: "unknown flag -id"},
		{name: "attached value on non-value short flag", line: `docker run -ix img`, wantErr: "unknown flag -ix"},
		{name: "arithmetic expansion", line: `docker run img $((1+2))`, wantErr: "unsupported shell syntax: arithmetic expansion"},
		{name: "brace expansion list", line: `docker run img a{1,2}`, wantErr: "unsupported shell syntax: brace expansion"},
		{name: "brace expansion sequence", line: `docker run img a{1..3}`, wantErr: "unsupported shell syntax: brace expansion"},
		{name: "param default", line: `docker run -e X=${X:-dflt} img`, wantErr: "unsupported shell syntax: parameter expansion"},
		{name: "param default quoted", line: `docker run -e "X=${X:-dflt}" img`, wantErr: "unsupported shell syntax: parameter expansion"},
		{name: "param alternate", line: `docker run -e X=${X:+word} img`, wantErr: "unsupported shell syntax: parameter expansion"},
		{name: "param length", line: `docker run img ${#X}`, wantErr: "unsupported shell syntax: parameter expansion"},
		{name: "param slice", line: `docker run img ${X:1:2}`, wantErr: "unsupported shell syntax: parameter expansion"},
		{name: "param replace", line: `docker run img ${X/a/b}`, wantErr: "unsupported shell syntax: parameter expansion"},
		{name: "param indirect", line: `docker run img ${!X}`, wantErr: "unsupported shell syntax: parameter expansion"},
		{name: "param index", line: `docker run img ${X[1]}`, wantErr: "unsupported shell syntax: parameter expansion"},
		{name: "special param at", line: `docker run img $@`, wantErr: "unsupported shell syntax: parameter expansion"},
		{name: "special param positional", line: `docker run img $1`, wantErr: "unsupported shell syntax: parameter expansion"},
		{name: "command substitution", line: `docker run img $(echo hi)`, wantErr: "unsupported shell syntax: command substitution"},
		{name: "command substitution backquote", line: "docker run img `echo hi`", wantErr: "unsupported shell syntax: command substitution"},
		{name: "command substitution quoted", line: `docker run img "$(echo hi)"`, wantErr: "unsupported shell syntax: command substitution"},
		{name: "process substitution in", line: `docker run img <(echo)`, wantErr: "unsupported shell syntax: process substitution"},
		{name: "process substitution out", line: `docker run img >(echo)`, wantErr: "unsupported shell syntax: process substitution"},
		{name: "tilde expansion", line: `docker run -v ~/x:/c img`, wantErr: "unsupported shell syntax: tilde expansion"},
		{name: "extglob", line: `docker run img @(a|b)`, wantErr: "unsupported shell syntax"},
		{name: "empty equals value", line: `docker run --name= img`, wantErr: "flag --name requires a value"},
		{
			name: "glob stays literal",
			line: `docker run img *.txt a?b`,
			want: dockerrun.Spec{Image: "img", Args: []string{"*.txt", "a?b"}},
		},
		{
			name: "tilde not at word start is literal",
			line: `docker run --volume=~/x:/c img`,
			want: dockerrun.Spec{Image: "img", Volumes: []string{"~/x:/c"}},
		},
		{
			name: "quoted braces and quoted tilde are literal",
			line: `docker run img "a{1,2}" '~' '$((1+2))' '${X:-d}'`,
			want: dockerrun.Spec{Image: "img", Args: []string{"a{1,2}", "~", "$((1+2))", "${X:-d}"}},
		},
		{
			name: "braces without comma are literal",
			line: `docker run img {} a{b}`,
			want: dockerrun.Spec{Image: "img", Args: []string{"{}", "a{b}"}},
		},
		{name: "tty -it", line: `docker run -it img`, wantErr: "tty is not supported"},
		{name: "tty -ti", line: `docker run -ti img`, wantErr: "tty is not supported"},
		{name: "tty -t", line: `docker run -t img`, wantErr: "tty is not supported"},
		{name: "tty --tty", line: `docker run --tty img`, wantErr: "tty is not supported"},
		{name: "env-file", line: `docker run --env-file .env img`, wantErr: "unsupported flag --env-file"},
		{name: "env-file joined", line: `docker run --env-file=.env img`, wantErr: "unsupported flag --env-file"},
		{name: "unknown flag", line: `docker run --privileged img`, wantErr: "unknown flag --privileged"},
		{name: "unknown short flag", line: `docker run -d img`, wantErr: "unknown flag -d"},
		{name: "missing image", line: `docker run --rm`, wantErr: "missing image"},
		{name: "missing image after terminator", line: `docker run --`, wantErr: "missing image"},
		{name: "not run", line: `docker build .`, wantErr: "expected run"},
		{name: "bare docker", line: `docker`, wantErr: "expected run"},
		{name: "not docker", line: `nerdctl run img`, wantErr: "expected docker or podman"},
		{name: "empty", line: ``, wantErr: "expected docker or podman"},
		{name: "value flag without value", line: `docker run -v`, wantErr: "flag -v requires a value"},
		{name: "long value flag without value", line: `docker run --name`, wantErr: "flag --name requires a value"},
		{name: "empty env key", line: `docker run -e =v img`, wantErr: "invalid env"},
		{name: "unterminated quote", line: `docker run "img`, wantErr: "parse"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := dockerrun.Parse(tt.line)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err=%v want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

// FuzzParse checks the (Spec, error) contract: no input may panic.
func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		`docker run img <(echo)`, `docker run img >(echo)`, `docker run img $((1+2))`,
		`docker run img ${X:-d}`, `docker run -e A=$B img`, `docker run img a{1,2}`,
		`docker run ~ img`, "docker run `x` img", `docker run img @(a|b) !(c) *(d)`,
		`docker run "`, `docker run 'x`, `docker run img $'\x41'`, `docker run img ;`,
		`docker run img | cat`, `docker run img & x`, `docker run img $(`, `docker run img ${`,
		`docker run img ${!X*}`, `docker run img ${X[@]}`, `docker run img <<EOF`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, line string) {
		_, _ = dockerrun.Parse(line)
	})
}

func TestParseInitAndMount(t *testing.T) {
	tests := []struct {
		name, line string
		want       []runtime.Mount
		wantErr    string
	}{
		{name: "init ignored", line: `docker run -i --init img`},
		{name: "init with value ignored", line: `docker run --init=true img`},
		{
			name: "bind mount",
			line: `docker run --mount type=bind,source=/h,target=/c,readonly img`,
			want: []runtime.Mount{{Type: "bind", Source: "/h", Target: "/c", ReadOnly: true}},
		},
		{
			name: "bind mount short keys joined",
			line: `docker run --mount=type=bind,src=/h,dst=/c img`,
			want: []runtime.Mount{{Type: "bind", Source: "/h", Target: "/c"}},
		},
		{
			name: "volume mount with readonly value and relabel",
			line: `podman run --mount type=volume,source=data,destination=/data,ro=true,relabel=private --mount type=bind,src=/a,target=/b,readonly=false,relabel=shared img`,
			want: []runtime.Mount{
				{Type: "volume", Source: "data", Target: "/data", ReadOnly: true, Relabel: "Z"},
				{Type: "bind", Source: "/a", Target: "/b", Relabel: "z"},
			},
		},
		{
			name: "type defaults to volume",
			line: `docker run --mount source=data,target=/data img`,
			want: []runtime.Mount{{Type: "volume", Source: "data", Target: "/data"}},
		},
		{name: "tmpfs unsupported", line: `docker run --mount type=tmpfs,target=/t img`, wantErr: `unsupported mount type "tmpfs"`},
		{name: "missing target", line: `docker run --mount type=bind,source=/h img`, wantErr: "target"},
		{name: "bind needs source", line: `docker run --mount type=bind,target=/c img`, wantErr: "source"},
		{name: "unknown key", line: `docker run --mount type=bind,source=/h,target=/c,bind-propagation=rshared img`, wantErr: `unsupported mount option "bind-propagation"`},
		{name: "bad readonly", line: `docker run --mount type=bind,source=/h,target=/c,readonly=maybe img`, wantErr: "readonly"},
		{name: "bad relabel", line: `docker run --mount type=bind,source=/h,target=/c,relabel=z img`, wantErr: "relabel"},
		{name: "mount without value", line: `docker run --mount`, wantErr: "flag --mount requires a value"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := dockerrun.Parse(tt.line)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err=%v want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Image != "img" || !reflect.DeepEqual(got.Mounts, tt.want) {
				t.Fatalf("got image %q mounts %+v, want %+v", got.Image, got.Mounts, tt.want)
			}
		})
	}
}
