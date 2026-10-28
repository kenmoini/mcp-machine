// Package buildinfo holds build metadata injected at link time via -ldflags -X.
package buildinfo

var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)
