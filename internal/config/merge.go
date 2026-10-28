package config

import "fmt"

// mergeServer refines a with b (a later admin definition of the same name),
// field by field. It returns a fresh spec sharing no maps, slices, or section
// pointers with a or b. It fails when both set a Type and the types differ.
func mergeServer(a, b ServerSpec) (ServerSpec, error) {
	if a.Type != "" && b.Type != "" && a.Type != b.Type {
		return ServerSpec{}, fmt.Errorf("server %q: type %q does not match %q", a.Name, b.Type, a.Type)
	}
	return mergeServerFields(a, b), nil
}

// cloneServer returns a deep copy of s.
func cloneServer(s ServerSpec) ServerSpec {
	return mergeServerFields(s, ServerSpec{})
}

// mergeServerFields merges every field of a and b without checking types.
// Merging with a zero value yields a deep copy, which is how cloneServer and
// the one-sided section cases avoid separate copy code.
func mergeServerFields(a, b ServerSpec) ServerSpec {
	return ServerSpec{
		Name:      mergeString(a.Name, b.Name),
		Type:      mergeString(a.Type, b.Type),
		Enabled:   mergeEnabled(a.Enabled, b.Enabled),
		Remote:    mergeRemote(a.Remote, b.Remote),
		Command:   mergeCommand(a.Command, b.Command),
		Build:     mergeBuild(a.Build, b.Build),
		Serve:     mergeServe(a.Serve, b.Serve),
		Container: mergeContainer(a.Container, b.Container),
		Env:       mergeMap(a.Env, b.Env),
	}
}

func mergeRemote(a, b *RemoteSpec) *RemoteSpec {
	if a == nil && b == nil {
		return nil
	}
	a, b = orZero(a), orZero(b)
	return &RemoteSpec{
		URL:       mergeString(a.URL, b.URL),
		Transport: mergeString(a.Transport, b.Transport),
		Headers:   mergeMap(a.Headers, b.Headers),
	}
}

func mergeCommand(a, b *CommandSpec) *CommandSpec {
	if a == nil && b == nil {
		return nil
	}
	a, b = orZero(a), orZero(b)
	return &CommandSpec{
		Command: mergeString(a.Command, b.Command),
		Args:    mergeSlice(a.Args, b.Args),
		Env:     mergeMap(a.Env, b.Env),
		Run:     mergeString(a.Run, b.Run),
	}
}

func mergeBuild(a, b *BuildSpec) *BuildSpec {
	if a == nil && b == nil {
		return nil
	}
	a, b = orZero(a), orZero(b)
	return &BuildSpec{
		Repo:          mergeString(a.Repo, b.Repo),
		Ref:           mergeString(a.Ref, b.Ref),
		Subdir:        mergeString(a.Subdir, b.Subdir),
		Containerfile: mergeString(a.Containerfile, b.Containerfile),
		Args:          mergeMap(a.Args, b.Args),
		Auth:          mergeAuth(a.Auth, b.Auth),
	}
}

func mergeAuth(a, b *GitAuth) *GitAuth {
	if a == nil && b == nil {
		return nil
	}
	a, b = orZero(a), orZero(b)
	return &GitAuth{
		Token:          mergeString(a.Token, b.Token),
		SSHKeyFile:     mergeString(a.SSHKeyFile, b.SSHKeyFile),
		KnownHostsFile: mergeString(a.KnownHostsFile, b.KnownHostsFile),
	}
}

func mergeServe(a, b *ServeSpec) *ServeSpec {
	if a == nil && b == nil {
		return nil
	}
	a, b = orZero(a), orZero(b)
	return &ServeSpec{
		Mode:      mergeString(a.Mode, b.Mode),
		Port:      mergeInt(a.Port, b.Port),
		Path:      mergeString(a.Path, b.Path),
		Transport: mergeString(a.Transport, b.Transport),
	}
}

func mergeContainer(a, b *ContainerOverrides) *ContainerOverrides {
	if a == nil && b == nil {
		return nil
	}
	a, b = orZero(a), orZero(b)
	return &ContainerOverrides{
		Image:   mergeString(a.Image, b.Image),
		Volumes: mergeSlice(a.Volumes, b.Volumes),
		Env:     mergeMap(a.Env, b.Env),
	}
}

// orZero returns p, or a pointer to a zero T when p is nil, so a section
// present on one side merges against an empty one and comes out as a copy.
func orZero[T any](p *T) *T {
	if p == nil {
		return new(T)
	}
	return p
}

// mergeString returns b when it is non-empty, else a.
func mergeString(a, b string) string {
	if b != "" {
		return b
	}
	return a
}

// mergeInt returns b when it is non-zero, else a.
func mergeInt(a, b int) int {
	if b != 0 {
		return b
	}
	return a
}

// mergeEnabled returns a copy of b when it is set, else a copy of a.
func mergeEnabled(a, b *bool) *bool {
	src := a
	if b != nil {
		src = b
	}
	if src == nil {
		return nil
	}
	v := *src
	return &v
}

// mergeMap returns a new map holding a's entries overlaid with b's, or nil
// when both are nil.
func mergeMap(a, b map[string]string) map[string]string {
	if a == nil && b == nil {
		return nil
	}
	m := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		m[k] = v
	}
	for k, v := range b {
		m[k] = v
	}
	return m
}

// mergeSlice returns a copy of b when it is non-nil (so an empty list
// clears a's), else a copy of a.
func mergeSlice(a, b []string) []string {
	src := a
	if b != nil {
		src = b
	}
	if src == nil {
		return nil
	}
	return append([]string{}, src...)
}
