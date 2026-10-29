//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kenmoini/mcp-machine/internal/manager"
	"github.com/kenmoini/mcp-machine/internal/runtime"
	"github.com/kenmoini/mcp-machine/internal/runtime/podman"
)

const (
	itAlpine  = "docker.io/library/alpine:3.20"
	itNetwork = "mcpm-it-net"
	itTag     = "localhost/mcp-machine/it:test"
	itTimeout = 3 * time.Minute
)

// randSuffix returns a short random hex string so reruns never collide on
// container names.
func randSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

// subCtx returns a per-subtest context bounded by itTimeout.
func subCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), itTimeout)
	t.Cleanup(cancel)
	return ctx
}

// trackContainer force-removes id when the test finishes, ignoring errors.
func trackContainer(t *testing.T, eng *podman.Engine, id string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = eng.RemoveContainer(ctx, id, true)
	})
}

func TestPodmanEngineEndToEnd(t *testing.T) {
	socket := os.Getenv("MCP_MACHINE_PODMAN_SOCKET")
	if socket == "" {
		t.Skip("MCP_MACHINE_PODMAN_SOCKET not set")
	}

	ctx := subCtx(t)
	eng, err := podman.New(ctx, socket)
	if err != nil {
		t.Fatalf("podman.New: %v", err)
	}
	suffix := randSuffix(t)
	labels := map[string]string{"io.mcp-machine/managed": "true"}

	t.Run("PingAndNetwork", func(t *testing.T) {
		ctx := subCtx(t)
		if err := eng.Ping(ctx); err != nil {
			t.Fatalf("Ping: %v", err)
		}
		// Idempotent: the network may pre-exist from an earlier run, and the
		// Engine has no network removal, so it is left in place afterwards.
		for i := 0; i < 2; i++ {
			if err := eng.EnsureNetwork(ctx, itNetwork); err != nil {
				t.Fatalf("EnsureNetwork #%d: %v", i+1, err)
			}
		}
	})

	t.Run("PullImage", func(t *testing.T) {
		ctx := subCtx(t)
		if err := eng.PullImage(ctx, itAlpine, nil, io.Discard); err != nil {
			t.Fatalf("PullImage: %v", err)
		}
		ok, err := eng.ImageExists(ctx, itAlpine)
		if err != nil {
			t.Fatalf("ImageExists: %v", err)
		}
		if !ok {
			t.Fatalf("ImageExists(%s) = false after pull", itAlpine)
		}
	})

	t.Run("AttachEcho", func(t *testing.T) {
		ctx := subCtx(t)
		id, err := eng.CreateContainer(ctx, runtime.ContainerSpec{
			Name:    "mcpm-it-cat-" + suffix,
			Image:   itAlpine,
			Command: []string{"cat"},
			Stdin:   true,
			Network: itNetwork,
			Labels:  labels,
		})
		if err != nil {
			t.Fatalf("CreateContainer: %v", err)
		}
		trackContainer(t, eng, id)

		// Attach must happen before start so no output is missed.
		rwc, err := eng.Attach(ctx, id)
		if err != nil {
			t.Fatalf("Attach: %v", err)
		}
		closed := false
		defer func() {
			if !closed {
				_ = rwc.Close()
			}
		}()

		if err := eng.StartContainer(ctx, id); err != nil {
			t.Fatalf("StartContainer: %v", err)
		}

		if _, err := rwc.Write([]byte("hello\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		type result struct {
			line string
			err  error
		}
		resc := make(chan result, 1)
		go func() {
			line, err := bufio.NewReader(rwc).ReadString('\n')
			resc <- result{line, err}
		}()
		select {
		case r := <-resc:
			if r.err != nil {
				t.Fatalf("ReadString: %v", r.err)
			}
			if r.line != "hello\n" {
				t.Fatalf("echo = %q, want %q", r.line, "hello\n")
			}
		case <-time.After(30 * time.Second):
			t.Fatal("timed out waiting for echo from container")
		}

		infos, err := eng.ListContainers(ctx, labels)
		if err != nil {
			t.Fatalf("ListContainers: %v", err)
		}
		found := false
		for _, info := range infos {
			if info.ID == id {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("ListContainers(%v) did not include %s", labels, id)
		}

		if _, err := eng.Logs(ctx, id, 10); err != nil {
			t.Fatalf("Logs: %v", err)
		}

		closed = true
		_ = rwc.Close()
		if err := eng.StopContainer(ctx, id, 2*time.Second); err != nil {
			t.Fatalf("StopContainer: %v", err)
		}
		waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if _, err := eng.Wait(waitCtx, id); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if err := eng.RemoveContainer(ctx, id, true); err != nil {
			t.Fatalf("RemoveContainer: %v", err)
		}
	})

	t.Run("ImageConfig", func(t *testing.T) {
		ctx := subCtx(t)
		cfg, err := eng.ImageConfig(ctx, itAlpine)
		if err != nil {
			t.Fatalf("ImageConfig: %v", err)
		}
		if !slices.Equal(cfg.Cmd, []string{"/bin/sh"}) || len(cfg.Entrypoint) != 0 {
			t.Errorf("alpine config = %+v, want Cmd [/bin/sh] and no entrypoint", cfg)
		}
		if _, err := eng.ImageConfig(ctx, "localhost/mcp-machine/does-not-exist:"+suffix); err == nil {
			t.Error("unknown image: no error")
		}
	})

	t.Run("SecretEnvDelivery", func(t *testing.T) {
		ctx := subCtx(t)
		img, err := eng.ImageConfig(ctx, itAlpine)
		if err != nil {
			t.Fatalf("ImageConfig: %v", err)
		}
		spec := runtime.ContainerSpec{
			Name:    "mcpm-it-secret-" + suffix,
			Image:   itAlpine,
			Command: []string{"sh", "-c", `echo "TOKEN=$TOKEN"; if [ -e ` + manager.DeliveryPath + ` ]; then echo kept; else echo gone; fi; stat -c %a /run/mcp-machine; sleep 30`},
			Stdin:   true,
			Network: itNetwork,
			Labels:  labels,
			Mounts:  []runtime.Mount{manager.SecretMount()},
		}
		if err := manager.WrapEntrypoint(&spec, img); err != nil {
			t.Fatalf("WrapEntrypoint: %v", err)
		}
		id, err := eng.CreateContainer(ctx, spec)
		if err != nil {
			t.Fatalf("CreateContainer: %v", err)
		}
		trackContainer(t, eng, id)
		rwc, err := eng.Attach(ctx, id)
		if err != nil {
			t.Fatalf("Attach: %v", err)
		}
		defer func() { _ = rwc.Close() }()
		if err := eng.StartContainer(ctx, id); err != nil {
			t.Fatalf("StartContainer: %v", err)
		}

		const secret = "it-works-" + "s3cr3t"
		file, err := manager.EnvFile(map[string]string{"TOKEN": secret})
		if err != nil {
			t.Fatal(err)
		}
		code, err := eng.Exec(ctx, id, manager.DeliverCommand(), bytes.NewReader(file))
		if err != nil || code != 0 {
			t.Fatalf("deliver: code %d, err %v", code, err)
		}

		lines := make(chan string, 3)
		go func() {
			r := bufio.NewReader(rwc)
			for i := 0; i < 3; i++ {
				l, err := r.ReadString('\n')
				if err != nil {
					lines <- "ERR " + err.Error()
					return
				}
				lines <- strings.TrimSpace(l)
			}
		}()
		want := []string{"TOKEN=" + secret, "gone", "1777"}
		for _, w := range want {
			select {
			case got := <-lines:
				if got != w {
					t.Fatalf("container printed %q, want %q", got, w)
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("timed out waiting for %q from the container", w)
			}
		}

		// Nothing of the secret is visible from outside the container.
		env, entrypoint, cmd, err := eng.InspectConfig(ctx, id)
		if err != nil {
			t.Fatalf("InspectConfig: %v", err)
		}
		for _, e := range env {
			if strings.Contains(e, secret) {
				t.Errorf("inspect Env holds the secret: %q", e)
			}
		}
		if len(entrypoint) == 0 || entrypoint[0] != "/bin/sh" {
			t.Errorf("inspect Entrypoint = %q, want the sh wrapper", entrypoint)
		}
		if strings.Contains(strings.Join(entrypoint, " ")+strings.Join(cmd, " "), secret) {
			t.Error("inspect Entrypoint/Cmd hold the secret")
		}
		code, err = eng.Exec(ctx, id, []string{"sh", "-c", "test ! -e " + manager.DeliveryPath}, nil)
		if err != nil || code != 0 {
			t.Errorf("env file still present: code %d, err %v", code, err)
		}
		// A non-zero exit is reported as such, not as an error.
		code, err = eng.Exec(ctx, id, []string{"sh", "-c", "echo oops >&2; exit 3"}, nil)
		if err != nil || code != 3 {
			t.Errorf("exit 3: code %d, err %v", code, err)
		}
	})

	t.Run("BuildImage", func(t *testing.T) {
		ctx := subCtx(t)
		dir := t.TempDir()
		content := "FROM " + itAlpine + "\nLABEL it=1\n"
		if err := os.WriteFile(filepath.Join(dir, "Containerfile"), []byte(content), 0o644); err != nil {
			t.Fatalf("write Containerfile: %v", err)
		}
		if _, err := eng.BuildImage(ctx, runtime.BuildRequest{
			ContextDir:    dir,
			Containerfile: "Containerfile",
			Tag:           itTag,
		}); err != nil {
			t.Fatalf("BuildImage: %v", err)
		}
		ok, err := eng.ImageExists(ctx, itTag)
		if err != nil {
			t.Fatalf("ImageExists: %v", err)
		}
		if !ok {
			t.Fatalf("ImageExists(%s) = false after build", itTag)
		}
	})

	t.Run("PullBadCredentials", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		err := eng.PullImage(ctx, "ghcr.io/kenmoini/does-not-exist:1",
			&runtime.RegistryAuth{Username: "u", Password: "p"}, io.Discard)
		if err == nil {
			t.Fatal("PullImage with bad credentials and nonexistent image succeeded; want error")
		}
		if ctx.Err() != nil {
			t.Fatalf("PullImage hung until the 60s timeout: %v", err)
		}
	})

	t.Run("ConnectNetwork", func(t *testing.T) {
		ctx := subCtx(t)
		id, err := eng.CreateContainer(ctx, runtime.ContainerSpec{
			Name:    "mcpm-it-sleep-" + suffix,
			Image:   itAlpine,
			Command: []string{"sleep", "60"},
			Labels:  labels,
		})
		if err != nil {
			t.Fatalf("CreateContainer: %v", err)
		}
		trackContainer(t, eng, id)
		if err := eng.StartContainer(ctx, id); err != nil {
			t.Fatalf("StartContainer: %v", err)
		}
		if err := eng.ConnectNetwork(ctx, id, itNetwork); err != nil {
			t.Fatalf("ConnectNetwork: %v", err)
		}
		info, err := eng.InspectContainer(ctx, id)
		if err != nil {
			t.Fatalf("InspectContainer: %v", err)
		}
		if !slices.Contains(info.Networks, itNetwork) {
			t.Fatalf("InspectContainer Networks = %v, want to contain %q", info.Networks, itNetwork)
		}
		// Stop quickly so the cleanup force-remove does not wait out the
		// default stop timeout on `sleep` (which ignores SIGTERM as PID 1).
		if err := eng.StopContainer(ctx, id, 1*time.Second); err != nil {
			t.Fatalf("StopContainer: %v", err)
		}
	})
}
