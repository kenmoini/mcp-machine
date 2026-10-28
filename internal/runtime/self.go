package runtime

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// SelfIDEnv names the environment variable that supplies MCP Machine's own
// container ID or name when it cannot be discovered (see ResolveSelfID).
const SelfIDEnv = "MCP_MACHINE_CONTAINER_ID"

// DetectSelf reports whether the process runs in a Podman container, which
// is the case when containerenvPath (normally /run/.containerenv) exists.
// id is the container ID recorded in the file's id= line. Podman writes that
// line only for privileged containers, so id is often empty even when
// inContainer is true; ResolveSelfID fills the gap.
func DetectSelf(containerenvPath string) (id string, inContainer bool) {
	f, err := os.Open(containerenvPath) //nolint:gosec // G304: caller passes the fixed /run/.containerenv path (a variable only for tests)
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
		return val, true
	}
	return "", true
}

// ResolveSelfID returns the ID of the container the process runs in. hint is
// the ID DetectSelf found, if any, and is returned as is when non-empty.
// Otherwise the host name (Podman sets it to the short container ID unless
// --hostname is given) is looked up with engine.InspectContainer, which
// accepts an ID, a short ID or a name. If that fails too, the SelfIDEnv
// environment variable is used. The error lists why every source failed.
func ResolveSelfID(ctx context.Context, engine Engine, hint string) (string, error) {
	if hint != "" {
		return hint, nil
	}
	var errs []error
	if host, err := os.Hostname(); err != nil {
		errs = append(errs, fmt.Errorf("host name: %w", err))
	} else if host == "" {
		errs = append(errs, errors.New("host name is empty"))
	} else if info, err := engine.InspectContainer(ctx, host); err != nil {
		errs = append(errs, fmt.Errorf("inspect container %q (the host name): %w", host, err))
	} else if info.ID != "" {
		return info.ID, nil
	} else {
		errs = append(errs, fmt.Errorf("inspect container %q (the host name): no ID", host))
	}
	if v := strings.TrimSpace(os.Getenv(SelfIDEnv)); v != "" {
		return v, nil
	}
	errs = append(errs, fmt.Errorf("%s is not set", SelfIDEnv))
	return "", fmt.Errorf("runtime: cannot determine own container ID: %w", errors.Join(errs...))
}
