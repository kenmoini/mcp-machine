package runtime

import (
	"bufio"
	"os"
	"strings"
)

// DetectSelf parses a Podman /run/.containerenv file and returns the
// container id of the process's own container. ok is false when the file is
// missing, unreadable, or has no non-empty id line.
func DetectSelf(containerenvPath string) (id string, ok bool) {
	f, err := os.Open(containerenvPath)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, val, found := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !found || strings.TrimSpace(key) != "id" {
			continue
		}
		val = strings.TrimSpace(val)
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			val = val[1 : len(val)-1]
		}
		if val == "" {
			return "", false
		}
		return val, true
	}
	return "", false
}
