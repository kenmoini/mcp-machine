package config

import (
	"reflect"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

func TestMergeServerScalarsNonZeroWins(t *testing.T) {
	a := ServerSpec{
		Name:   "x",
		Type:   "remote",
		Remote: &RemoteSpec{URL: "https://a/", Transport: "http"},
		Serve:  &ServeSpec{Port: 3000, Path: "/mcp"},
	}
	b := ServerSpec{
		Name:   "x",
		Remote: &RemoteSpec{Transport: "sse"},
		Serve:  &ServeSpec{Port: 0, Mode: "stdio"},
	}
	got, err := mergeServer(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != "remote" {
		t.Errorf("Type = %q, want remote", got.Type)
	}
	if got.Remote.URL != "https://a/" {
		t.Errorf("Remote.URL = %q, want a's value", got.Remote.URL)
	}
	if got.Remote.Transport != "sse" {
		t.Errorf("Remote.Transport = %q, want sse", got.Remote.Transport)
	}
	if got.Serve.Port != 3000 || got.Serve.Path != "/mcp" || got.Serve.Mode != "stdio" {
		t.Errorf("Serve = %+v, want port 3000, path /mcp, mode stdio", got.Serve)
	}
}

func TestMergeServerTypeFilledFromB(t *testing.T) {
	got, err := mergeServer(ServerSpec{Name: "x"}, ServerSpec{Name: "x", Type: "command"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != "command" {
		t.Errorf("Type = %q, want command", got.Type)
	}
}

func TestMergeServerEnabledPointer(t *testing.T) {
	cases := []struct {
		name string
		a, b *bool
		want *bool
	}{
		{"nil then false", nil, boolPtr(false), boolPtr(false)},
		{"false then nil", boolPtr(false), nil, boolPtr(false)},
		{"false then true", boolPtr(false), boolPtr(true), boolPtr(true)},
		{"both nil", nil, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := mergeServer(ServerSpec{Name: "x", Enabled: c.a}, ServerSpec{Name: "x", Enabled: c.b})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Enabled, c.want) {
				t.Errorf("Enabled = %v, want %v", got.Enabled, c.want)
			}
		})
	}
}

func TestMergeServerEnabledDoesNotAlias(t *testing.T) {
	a := ServerSpec{Name: "x", Enabled: boolPtr(false)}
	got, err := mergeServer(a, ServerSpec{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	*got.Enabled = true
	if *a.Enabled {
		t.Error("mutating result.Enabled changed a.Enabled")
	}
}

func TestMergeServerMapsKeyWise(t *testing.T) {
	a := ServerSpec{Name: "x", Env: map[string]string{"A": "1", "B": "2"}}
	b := ServerSpec{Name: "x", Env: map[string]string{"B": "3", "C": "4"}}
	got, err := mergeServer(a, b)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "1", "B": "3", "C": "4"}
	if !reflect.DeepEqual(got.Env, want) {
		t.Fatalf("Env = %v, want %v", got.Env, want)
	}
	got.Env["A"] = "changed"
	if a.Env["A"] != "1" {
		t.Error("mutating result.Env changed a.Env")
	}
}

func TestMergeServerMapsNilWhenBothNil(t *testing.T) {
	got, err := mergeServer(ServerSpec{Name: "x"}, ServerSpec{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Env != nil {
		t.Errorf("Env = %v, want nil", got.Env)
	}
}

func TestMergeServerNestedMaps(t *testing.T) {
	a := ServerSpec{Name: "x",
		Remote:    &RemoteSpec{Headers: map[string]string{"H1": "a"}},
		Command:   &CommandSpec{Env: map[string]string{"E1": "a"}},
		Container: &ContainerOverrides{Env: map[string]string{"C1": "a"}},
		Build:     &BuildSpec{Args: map[string]string{"B1": "a"}},
	}
	b := ServerSpec{Name: "x",
		Remote:    &RemoteSpec{Headers: map[string]string{"H2": "b"}},
		Command:   &CommandSpec{Env: map[string]string{"E2": "b"}},
		Container: &ContainerOverrides{Env: map[string]string{"C2": "b"}},
		Build:     &BuildSpec{Args: map[string]string{"B2": "b"}},
	}
	got, err := mergeServer(a, b)
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name string
		got  map[string]string
		want map[string]string
	}{
		{"Remote.Headers", got.Remote.Headers, map[string]string{"H1": "a", "H2": "b"}},
		{"Command.Env", got.Command.Env, map[string]string{"E1": "a", "E2": "b"}},
		{"Container.Env", got.Container.Env, map[string]string{"C1": "a", "C2": "b"}},
		{"Build.Args", got.Build.Args, map[string]string{"B1": "a", "B2": "b"}},
	}
	for _, c := range checks {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestMergeServerSlicesReplace(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want []string
	}{
		{"b nil keeps a", []string{"x", "y"}, nil, []string{"x", "y"}},
		{"b empty clears", []string{"x", "y"}, []string{}, []string{}},
		{"b replaces", []string{"x", "y"}, []string{"z"}, []string{"z"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := ServerSpec{Name: "x", Command: &CommandSpec{Args: c.a}, Container: &ContainerOverrides{Volumes: c.a}}
			b := ServerSpec{Name: "x", Command: &CommandSpec{Args: c.b}, Container: &ContainerOverrides{Volumes: c.b}}
			got, err := mergeServer(a, b)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Command.Args, c.want) {
				t.Errorf("Command.Args = %#v, want %#v", got.Command.Args, c.want)
			}
			if !reflect.DeepEqual(got.Container.Volumes, c.want) {
				t.Errorf("Container.Volumes = %#v, want %#v", got.Container.Volumes, c.want)
			}
		})
	}
}

func TestMergeServerSectionsRecursive(t *testing.T) {
	a := ServerSpec{Name: "x", Build: &BuildSpec{Repo: "r", Auth: &GitAuth{Token: "t"}}}
	b := ServerSpec{Name: "x", Build: &BuildSpec{Ref: "main", Auth: &GitAuth{SSHKeyFile: "/k", KnownHostsFile: "/kh"}}}
	got, err := mergeServer(a, b)
	if err != nil {
		t.Fatal(err)
	}
	want := &BuildSpec{Repo: "r", Ref: "main", Auth: &GitAuth{Token: "t", SSHKeyFile: "/k", KnownHostsFile: "/kh"}}
	if !reflect.DeepEqual(got.Build, want) {
		t.Fatalf("Build = %+v (auth %+v), want %+v (auth %+v)", got.Build, got.Build.Auth, want, want.Auth)
	}
}

func TestMergeServerOneSidedSections(t *testing.T) {
	a := ServerSpec{Name: "x", Remote: &RemoteSpec{URL: "u"}, Build: &BuildSpec{Repo: "r", Auth: &GitAuth{Token: "t"}}}
	b := ServerSpec{Name: "x", Command: &CommandSpec{Command: "npx"}, Serve: &ServeSpec{Port: 1}, Container: &ContainerOverrides{Image: "i"}, Build: &BuildSpec{Ref: "main"}}
	got, err := mergeServer(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Remote == nil || got.Remote.URL != "u" || got.Command == nil || got.Command.Command != "npx" ||
		got.Serve == nil || got.Serve.Port != 1 || got.Container == nil || got.Container.Image != "i" {
		t.Fatalf("one-sided sections lost: %+v", got)
	}
	if got.Build.Auth == nil || got.Build.Auth.Token != "t" || got.Build.Repo != "r" || got.Build.Ref != "main" {
		t.Fatalf("Build = %+v, want repo r, ref main, auth token t", got.Build)
	}
	if got.Build.Auth == a.Build.Auth {
		t.Error("result.Build.Auth aliases a.Build.Auth")
	}
	// Sections absent on both sides stay nil.
	empty, err := mergeServer(ServerSpec{Name: "x"}, ServerSpec{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if empty.Remote != nil || empty.Command != nil || empty.Build != nil || empty.Serve != nil || empty.Container != nil {
		t.Errorf("absent sections should stay nil: %+v", empty)
	}
}

func TestMergeServerTypeMismatch(t *testing.T) {
	_, err := mergeServer(ServerSpec{Name: "x", Type: "remote"}, ServerSpec{Name: "x", Type: "command"})
	if err == nil {
		t.Fatal("want error")
	}
	want := `server "x": type "command" does not match "remote"`
	if err.Error() != want {
		t.Fatalf("err = %q, want %q", err.Error(), want)
	}
}

func fullServer() ServerSpec {
	return ServerSpec{
		Name:    "x",
		Type:    "command",
		Enabled: boolPtr(true),
		Env:     map[string]string{"K": "v"},
		Remote:  &RemoteSpec{URL: "u", Transport: "http", Headers: map[string]string{"H": "v"}},
		Command: &CommandSpec{Command: "npx", Args: []string{"a", "b"}, Env: map[string]string{"E": "v"}, Run: "r"},
		Build: &BuildSpec{Repo: "r", Ref: "m", Subdir: "s", Containerfile: "c",
			Args: map[string]string{"B": "v"},
			Auth: &GitAuth{Token: "t", SSHKeyFile: "k", KnownHostsFile: "kh"}},
		Serve:     &ServeSpec{Mode: "m", Port: 1, Path: "/p", Transport: "t"},
		Container: &ContainerOverrides{Image: "i", Volumes: []string{"v1"}, Env: map[string]string{"C": "v"}},
	}
}

func TestMergeServerDoesNotAlias(t *testing.T) {
	a, b := fullServer(), fullServer()
	got, err := mergeServer(a, b)
	if err != nil {
		t.Fatal(err)
	}
	got.Remote.Headers["H"] = "changed"
	got.Command.Args[0] = "changed"
	got.Command.Env["E"] = "changed"
	got.Container.Volumes[0] = "changed"
	got.Build.Args["B"] = "changed"
	got.Build.Auth.Token = "changed"
	got.Serve.Port = 99
	got.Env["K"] = "changed"
	*got.Enabled = false
	if !reflect.DeepEqual(a, fullServer()) {
		t.Errorf("a was modified: %+v", a)
	}
	if !reflect.DeepEqual(b, fullServer()) {
		t.Errorf("b was modified: %+v", b)
	}
}

func TestMergeServerDoesNotAliasOneSided(t *testing.T) {
	a, b := fullServer(), ServerSpec{Name: "x"}
	got, err := mergeServer(a, b)
	if err != nil {
		t.Fatal(err)
	}
	got.Remote.Headers["H"] = "changed"
	got.Command.Args[0] = "changed"
	got.Build.Auth.Token = "changed"
	got.Container.Volumes[0] = "changed"
	if !reflect.DeepEqual(a, fullServer()) {
		t.Errorf("a was modified: %+v", a)
	}
	b2 := fullServer()
	got2, err := mergeServer(ServerSpec{Name: "x"}, b2)
	if err != nil {
		t.Fatal(err)
	}
	got2.Command.Args[0] = "changed"
	got2.Env["K"] = "changed"
	got2.Build.Auth.Token = "changed"
	if !reflect.DeepEqual(b2, fullServer()) {
		t.Errorf("b was modified: %+v", b2)
	}
}

func TestCloneServerDeep(t *testing.T) {
	orig := fullServer()
	c := cloneServer(orig)
	if !reflect.DeepEqual(c, orig) {
		t.Fatalf("clone differs: %+v vs %+v", c, orig)
	}
	c.Env["K"] = "changed"
	c.Remote.Headers["H"] = "changed"
	c.Remote.URL = "changed"
	c.Command.Args[0] = "changed"
	c.Command.Env["E"] = "changed"
	c.Build.Args["B"] = "changed"
	c.Build.Auth.KnownHostsFile = "changed"
	c.Serve.Port = 99
	c.Container.Volumes[0] = "changed"
	c.Container.Env["C"] = "changed"
	*c.Enabled = false
	if !reflect.DeepEqual(orig, fullServer()) {
		t.Errorf("original modified: %+v", orig)
	}
}

func TestCloneServerNilSections(t *testing.T) {
	c := cloneServer(ServerSpec{Name: "x", Type: "remote"})
	if c.Remote != nil || c.Env != nil || c.Enabled != nil {
		t.Errorf("nil fields should stay nil: %+v", c)
	}
}
