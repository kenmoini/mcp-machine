// Package store holds container label helpers and the in-memory status
// registry that the manager writes and the web UI reads.
package store

// Label keys stamped on every container managed by MCP Machine.
const (
	LabelManaged   = "io.mcp-machine/managed"
	LabelServer    = "io.mcp-machine/server"
	LabelHash      = "io.mcp-machine/hash"
	LabelTransport = "io.mcp-machine/transport"
)

// ContainerPrefix is prepended to a server name to form its container name.
const ContainerPrefix = "mcpm-"

// LabelInfo is the decoded form of the labels on a managed container.
type LabelInfo struct {
	Name, Hash, Transport string
}

// Labels returns the label set for a managed container.
func Labels(name, hash, transport string) map[string]string {
	return map[string]string{
		LabelManaged:   "true",
		LabelServer:    name,
		LabelHash:      hash,
		LabelTransport: transport,
	}
}

// ParseLabels decodes container labels. It reports false unless the container
// is marked managed and carries a non-empty server name.
func ParseLabels(m map[string]string) (LabelInfo, bool) {
	if m[LabelManaged] != "true" || m[LabelServer] == "" {
		return LabelInfo{}, false
	}
	return LabelInfo{
		Name:      m[LabelServer],
		Hash:      m[LabelHash],
		Transport: m[LabelTransport],
	}, true
}

// ContainerName returns the container name for a server.
func ContainerName(server string) string { return ContainerPrefix + server }
