package sources

import (
	"strings"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/runtime"
)

// RegistryAuthFor returns the credentials for imageRef: the Registry entry with
// the longest value that prefixes the ref (on path-segment boundaries), or nil.
// A ref without an explicit registry host is treated as living on docker.io.
func RegistryAuthFor(regs []config.Registry, imageRef string) *runtime.RegistryAuth {
	full := normaliseRef(imageRef)
	var best *config.Registry
	for i := range regs {
		r := &regs[i]
		reg := strings.TrimSuffix(r.Registry, "/")
		if reg == "" || !hasSegmentPrefix(full, reg) {
			continue
		}
		if best == nil || len(reg) > len(strings.TrimSuffix(best.Registry, "/")) {
			best = r
		}
	}
	if best == nil {
		return nil
	}
	return &runtime.RegistryAuth{Username: best.Username, Password: best.Password}
}

// normaliseRef prepends docker.io when the first path component is not a host.
func normaliseRef(ref string) string {
	first, _, _ := strings.Cut(ref, "/")
	if strings.Contains(ref, "/") && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return ref
	}
	return "docker.io/" + ref
}

// hasSegmentPrefix reports whether s equals prefix or starts with prefix
// followed by a path or tag/digest separator.
func hasSegmentPrefix(s, prefix string) bool {
	if !strings.HasPrefix(s, prefix) {
		return false
	}
	if len(s) == len(prefix) {
		return true
	}
	switch s[len(prefix)] {
	case '/', ':', '@':
		return true
	}
	return false
}
