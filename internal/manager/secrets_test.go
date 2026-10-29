package manager_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/manager"
	"github.com/kenmoini/mcp-machine/internal/runtime"
	"github.com/kenmoini/mcp-machine/internal/sources"
	"github.com/kenmoini/mcp-machine/internal/store"
	"github.com/kenmoini/mcp-machine/internal/vault"
)

// secretResolved is server a as a stdio container with one env var from
// Vault and one plain one.
func secretResolved() sources.Resolved {
	res := stdioResolved()
	res.Container.Env = map[string]string{"PLAIN": "p"}
	res.Container.SecretEnv = map[string]string{"TOKEN": "${vault:kv/data/a#tok}"}
	return res
}

func newVaultFake() *vault.Fake {
	f := &vault.Fake{}
	f.Set("kv/data/a", "tok", "s3cr3t")
	return f
}

// secretHarness is a harness whose server a has a Vault secret and whose
// engine knows img:1's entrypoint.
func secretHarness(t *testing.T, opts manager.Options) (*harness, *vault.Fake) {
	t.Helper()
	fv := newVaultFake()
	if opts.Vault == nil {
		opts.Vault = fv
	}
	h := newHarness(t, opts)
	h.res.res = secretResolved()
	h.cfg.Servers[0].Env = map[string]string{"TOKEN": "${vault:kv/data/a#tok}"}
	h.eng.SetImageConfig("img:1", runtime.ImageConfig{Entrypoint: []string{"docker-entrypoint.sh"}, Cmd: []string{"node"}})
	return h, fv
}

func TestSecretEnvCreatesWrapperAndTmpfs(t *testing.T) {
	h, fv := secretHarness(t, manager.Options{})
	st, _ := h.ready()

	spec, ok := h.eng.Spec(st.ContainerID)
	if !ok {
		t.Fatal("no container")
	}
	if len(spec.Entrypoint) != 4 || spec.Entrypoint[0] != "/bin/sh" || spec.Entrypoint[1] != "-c" || spec.Entrypoint[3] != "mcp-machine" {
		t.Errorf("entrypoint = %q, want the sh wrapper", spec.Entrypoint)
	}
	if want := []string{"docker-entrypoint.sh", "npx", "-y", "server"}; !slices.Equal(spec.Command, want) {
		t.Errorf("command = %q, want %q", spec.Command, want)
	}
	if _, leaked := spec.Env["TOKEN"]; leaked || spec.Env["PLAIN"] != "p" {
		t.Errorf("env = %v: the secret must not be in the create spec", spec.Env)
	}
	if len(spec.SecretEnv) != 0 {
		t.Errorf("SecretEnv reached the engine: %v", spec.SecretEnv)
	}
	tmpfs := false
	for _, m := range spec.Mounts {
		if m.Type == "tmpfs" && m.Target == "/run/mcp-machine" && strings.Contains(m.Options, "mode=1777") {
			tmpfs = true
		}
	}
	if !tmpfs {
		t.Errorf("mounts = %+v, want the secrets tmpfs", spec.Mounts)
	}

	execs := h.eng.Execs()
	if len(execs) != 1 {
		t.Fatalf("execs = %+v, want one delivery", execs)
	}
	if execs[0].ID != st.ContainerID || string(execs[0].Stdin) != "TOKEN='s3cr3t'\n" {
		t.Errorf("delivery = %+v", execs[0])
	}
	if !strings.HasPrefix(execs[0].Cmd[2], "umask 077 && cat > /run/mcp-machine/env.tmp") {
		t.Errorf("delivery command = %q", execs[0].Cmd)
	}
	if calls := h.eng.Calls(); !inOrder(calls, []string{"ImageConfig", "CreateContainer", "Attach", "StartContainer", "Exec"}) {
		t.Errorf("calls = %v, want ImageConfig, CreateContainer, Attach, StartContainer, Exec in order", calls)
	}
	if reads := fv.Reads(); !slices.Equal(reads, []string{"kv/data/a"}) {
		t.Errorf("vault reads = %v", reads)
	}
	// The container label hash holds the reference, never the value.
	if strings.Contains(spec.Labels[store.LabelHash], "s3cr3t") {
		t.Error("hash label holds the secret")
	}
}

func TestNoSecretEnvSpecUnchanged(t *testing.T) {
	h := newHarness(t, manager.Options{Vault: newVaultFake()})
	h.eng.SetImageConfig("img:1", runtime.ImageConfig{Entrypoint: []string{"docker-entrypoint.sh"}})
	st, _ := h.ready()
	spec, _ := h.eng.Spec(st.ContainerID)
	if spec.Entrypoint != nil {
		t.Errorf("entrypoint set without secrets: %q", spec.Entrypoint)
	}
	for _, m := range spec.Mounts {
		if m.Type == "tmpfs" {
			t.Errorf("tmpfs mounted without secrets: %+v", m)
		}
	}
	for _, m := range []string{"Exec", "ImageConfig"} {
		if slices.Contains(h.eng.Calls(), m) {
			t.Errorf("%s called without secrets", m)
		}
	}
}

func TestSecretEnvHTTPDeliversBeforeProbe(t *testing.T) {
	h, _ := secretHarness(t, manager.Options{
		EndpointFunc: func(runtime.ContainerInfo, sources.HTTPEndpoint) (string, error) {
			return "", errors.New("test: no endpoint")
		},
	})
	res := httpResolved()
	res.Container.SecretEnv = map[string]string{"TOKEN": "${vault:kv/data/a#tok}"}
	h.res.res = res
	h.apply()
	st := waitFor(t, h.reg, "a", "endpoint failure", func(s store.Status, ok bool) bool {
		return ok && s.State == store.StateFailed && strings.Contains(s.Error, "test: no endpoint")
	})
	if calls := h.eng.Calls(); !inOrder(calls, []string{"CreateContainer", "StartContainer", "Exec", "InspectContainer"}) {
		t.Errorf("calls = %v, want delivery between start and the endpoint inspect", calls)
	}
	if slices.Contains(h.eng.Calls(), "Attach") {
		t.Error("an http container was attached")
	}
	if execs := h.eng.Execs(); len(execs) != 1 || execs[0].ID != st.ContainerID {
		t.Errorf("execs = %+v", execs)
	}
}

func TestSecretEnvNilReaderFails(t *testing.T) {
	h := newHarness(t, manager.Options{})
	h.res.res = secretResolved()
	h.eng.MarkImage("img:1")
	h.apply()
	st := waitState(t, h.reg, "a", store.StateFailed)
	if !strings.Contains(st.Error, "vault is not configured") {
		t.Errorf("error = %q", st.Error)
	}
	if slices.Contains(h.eng.Calls(), "CreateContainer") {
		t.Error("a container was created without its secrets")
	}
}

func TestSecretEnvVaultReadFailsBeforeCreate(t *testing.T) {
	fv := &vault.Fake{} // kv/data/a is missing: 404
	h, _ := secretHarness(t, manager.Options{Vault: fv})
	h.apply()
	st := waitState(t, h.reg, "a", store.StateFailed)
	want := "secrets: ${vault:kv/data/a#tok}: vault: read kv/data/a: 404 not found"
	if st.Error != want {
		t.Errorf("error = %q, want %q", st.Error, want)
	}
	if slices.Contains(h.eng.Calls(), "CreateContainer") {
		t.Error("a container was created although Vault failed")
	}

	// A missing key is reported by name.
	fv.Set("kv/data/a", "other", "x")
	if err := h.m.Start(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	st = waitFor(t, h.reg, "a", "missing key", func(s store.Status, ok bool) bool {
		return ok && s.State == store.StateFailed && strings.Contains(s.Error, `has no key "tok"`)
	})
	_ = st
}

func TestSecretEnvResumeRedelivers(t *testing.T) {
	h, fv := secretHarness(t, manager.Options{})
	st, _ := h.ready()
	ctx := context.Background()

	if err := h.m.Stop(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	waitState(t, h.reg, "a", store.StateStopped)
	fv.Set("kv/data/a", "tok", "rotated")
	if err := h.m.Start(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	serveBackend(t, h.eng, st.ContainerID)
	got := waitState(t, h.reg, "a", store.StateReady)
	if got.ContainerID != st.ContainerID {
		t.Errorf("resume replaced the container: %s -> %s", st.ContainerID, got.ContainerID)
	}
	execs := h.eng.Execs()
	if len(execs) != 2 || string(execs[1].Stdin) != "TOKEN='rotated'\n" {
		t.Errorf("execs = %+v, want a second delivery with the rotated value", execs)
	}
	if n := h.count("CreateContainer"); n != 1 {
		t.Errorf("CreateContainer called %d times, want 1", n)
	}
}

func TestAdoptStdioWithSecretEnvResolvesAndDelivers(t *testing.T) {
	h, _ := secretHarness(t, manager.Options{})
	h.eng.Seed(runtime.ContainerInfo{
		ID:     "seed-1",
		Name:   store.ContainerName("a"),
		Image:  "img:1",
		State:  "exited",
		Labels: store.Labels("a", h.hash(), "stdio"),
	})
	h.apply()
	serveBackend(t, h.eng, "seed-1")
	st := waitState(t, h.reg, "a", store.StateReady)
	if st.ContainerID != "seed-1" {
		t.Errorf("ContainerID = %q, want the adopted seed-1", st.ContainerID)
	}
	execs := h.eng.Execs()
	if len(execs) != 1 || execs[0].ID != "seed-1" || string(execs[0].Stdin) != "TOKEN='s3cr3t'\n" {
		t.Errorf("execs = %+v, want one delivery into seed-1", execs)
	}
	if calls := h.eng.Calls(); !inOrder(calls, []string{"Attach", "StartContainer", "Exec"}) || slices.Contains(calls, "CreateContainer") {
		t.Errorf("calls = %v", calls)
	}
}

func TestAdoptHTTPRunningSkipsDelivery(t *testing.T) {
	h, _ := secretHarness(t, manager.Options{
		EndpointFunc: func(runtime.ContainerInfo, sources.HTTPEndpoint) (string, error) {
			return "", errors.New("test: no endpoint")
		},
	})
	res := httpResolved()
	res.Container.SecretEnv = map[string]string{"TOKEN": "${vault:kv/data/a#tok}"}
	h.res.res = res
	h.eng.Seed(runtime.ContainerInfo{
		ID: "seed-1", Name: store.ContainerName("a"), Image: "img:1", State: "running",
		Labels: store.Labels("a", h.hash(), "http"),
	})
	h.apply()
	waitFor(t, h.reg, "a", "endpoint failure", func(s store.Status, ok bool) bool {
		return ok && s.State == store.StateFailed && strings.Contains(s.Error, "test: no endpoint")
	})
	if execs := h.eng.Execs(); len(execs) != 0 {
		t.Errorf("a running adopted container got a delivery: %+v", execs)
	}
	if slices.Contains(h.eng.Calls(), "StartContainer") {
		t.Error("a running container was started")
	}
}

func TestDeliverExecFailureRetriesRecreate(t *testing.T) {
	h, _ := secretHarness(t, manager.Options{MaxRetries: 3})
	h.eng.FailNext("Exec", errors.New("cat: can't create /run/mcp-machine/env.tmp"))
	h.apply()
	st := waitState(t, h.reg, "a", store.StateFailed)
	if !strings.Contains(st.Error, "deliver secrets: cat: can't create") {
		t.Errorf("error = %q", st.Error)
	}
	h.clk.Advance(time.Second) // first backoff
	serveNext(t, h, st.ContainerID)
	got := waitState(t, h.reg, "a", store.StateReady)
	if got.ContainerID == st.ContainerID {
		t.Error("retry reused the container whose delivery failed")
	}
	if n := h.count("Exec"); n != 2 {
		t.Errorf("Exec called %d times, want 2", n)
	}

	// A non-zero exit from the delivery command is a failure too.
	h.eng.SetExecExit(1)
	if err := h.m.Restart(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, h.reg, "a", "exit 1", func(s store.Status, ok bool) bool {
		return ok && s.State == store.StateFailed && strings.Contains(s.Error, "deliver secrets: exit 1")
	})
}

func TestDeliverFailurePrefersContainerExit(t *testing.T) {
	// An image without /bin/sh exits at once; the exit explains the failure
	// better than the exec error does.
	h, _ := secretHarness(t, manager.Options{MaxRetries: -1})
	h.eng.OnStart(func(id string) {
		h.eng.SetLogs(id, []runtime.LogLine{{Stream: "stderr", Text: "exec /bin/sh: no such file or directory"}})
		h.eng.Exit(id, 127)
	})
	h.apply()
	st := waitState(t, h.reg, "a", store.StateFailed)
	if !strings.Contains(st.Error, "container exited with code 127") || !strings.Contains(st.Error, "no such file") {
		t.Errorf("error = %q, want the container's exit", st.Error)
	}
}

func TestRemoteHeadersResolvedPerAttempt(t *testing.T) {
	var (
		mu    sync.Mutex
		auths []string
	)
	backend := streamableHandler(pingServer())
	useInProcessHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		backend.ServeHTTP(w, r)
	}))
	fv := newVaultFake()
	h := newHarness(t, manager.Options{Vault: fv})
	h.cfg.Servers[0] = config.ServerSpec{Name: "a", Type: "remote", Remote: &config.RemoteSpec{URL: "http://remote.example/mcp", Transport: "http"}}
	h.res.res = sources.Resolved{
		Kind: sources.KindRemote,
		Remote: &sources.RemoteEndpoint{
			URL:       "http://remote.example/mcp",
			Transport: "http",
			Headers:   map[string]string{"Authorization": "Bearer ${vault:kv/data/a#tok}", "X-Plain": "p"},
		},
	}
	h.apply()
	waitState(t, h.reg, "a", store.StateReady)
	mu.Lock()
	first := slices.Clone(auths)
	mu.Unlock()
	if len(first) == 0 || first[0] != "Bearer s3cr3t" {
		t.Fatalf("Authorization seen = %q, want the resolved secret", first)
	}
	// The resolved endpoint is not written back: the template stays.
	if got := h.res.res.Remote.Headers["Authorization"]; got != "Bearer ${vault:kv/data/a#tok}" {
		t.Errorf("template changed: %q", got)
	}

	fv.Set("kv/data/a", "tok", "rotated")
	if err := h.m.Restart(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, h.reg, "a", "ready again", func(s store.Status, ok bool) bool {
		mu.Lock()
		defer mu.Unlock()
		return ok && s.State == store.StateReady && slices.Contains(auths, "Bearer rotated")
	})

	// Without Vault the dial is refused before any request.
	h2 := newHarness(t, manager.Options{})
	h2.cfg.Servers[0] = h.cfg.Servers[0]
	h2.res.res = h.res.res
	h2.apply()
	st := waitState(t, h2.reg, "a", store.StateFailed)
	if !strings.Contains(st.Error, "headers: vault is not configured") {
		t.Errorf("error = %q", st.Error)
	}
}

func TestPullAuthResolvedFromVault(t *testing.T) {
	h := newHarness(t, manager.Options{Vault: newVaultFake()}) // img:1 is not present: it is pulled
	res := stdioResolved()
	res.Image.Auth = &runtime.RegistryAuth{Username: "u", Password: "${vault:kv/data/a#tok}"}
	h.res.res = res
	h.ready()
	auths := h.eng.PullAuths()
	if len(auths) != 1 || auths[0] == nil || auths[0].Password != "s3cr3t" || auths[0].Username != "u" {
		t.Errorf("pull auths = %+v, want the resolved password", auths)
	}
	if res.Image.Auth.Password != "${vault:kv/data/a#tok}" {
		t.Error("the resolved template was written back")
	}

	h2 := newHarness(t, manager.Options{})
	h2.res.res = res
	h2.apply()
	st := waitState(t, h2.reg, "a", store.StateFailed)
	if !strings.Contains(st.Error, "pull img:1: registry credentials: vault is not configured") {
		t.Errorf("error = %q", st.Error)
	}
}
