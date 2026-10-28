package cli

import "testing"

// SetLegacyConfigPath points the deprecated-location fallback at path for
// the duration of t.
func SetLegacyConfigPath(t testing.TB, path string) {
	t.Helper()
	old := legacyConfigPath
	legacyConfigPath = path
	t.Cleanup(func() { legacyConfigPath = old })
}
