package remote

import (
	"reflect"
	"testing"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/sources"
)

func TestRemoteResolve(t *testing.T) {
	hdr := map[string]string{"Authorization": "Bearer t"}
	spec := config.ServerSpec{Name: "r", Remote: &config.RemoteSpec{URL: "https://example.com/mcp", Headers: hdr}}
	res, err := Resolve(&config.Config{}, spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != sources.KindRemote || res.Remote == nil {
		t.Fatalf("res = %+v", res)
	}
	if res.Remote.URL != "https://example.com/mcp" || res.Remote.Transport != "auto" {
		t.Fatalf("remote = %+v", res.Remote)
	}
	if !reflect.DeepEqual(res.Remote.Headers, hdr) {
		t.Fatalf("headers = %v", res.Remote.Headers)
	}
	hdr["X"] = "mutated"
	if _, ok := res.Remote.Headers["X"]; ok {
		t.Fatal("headers not copied")
	}

	spec.Remote.Transport = "sse"
	res, err = Resolve(&config.Config{}, spec)
	if err != nil || res.Remote.Transport != "sse" {
		t.Fatalf("transport = %+v, err %v", res.Remote, err)
	}
}

func TestRemoteResolveErrors(t *testing.T) {
	for _, u := range []string{"ftp://x", "", "example.com/mcp", "http://", "://bad"} {
		spec := config.ServerSpec{Name: "r", Remote: &config.RemoteSpec{URL: u}}
		if _, err := Resolve(&config.Config{}, spec); err == nil {
			t.Errorf("url %q: want error", u)
		}
	}
	if _, err := Resolve(&config.Config{}, config.ServerSpec{Name: "r"}); err == nil {
		t.Error("nil remote: want error")
	}
}

func TestRemoteResolveKeepsVaultHeaderTemplate(t *testing.T) {
	res, err := Resolve(nil, config.ServerSpec{Name: "r", Remote: &config.RemoteSpec{
		URL: "https://x.example/mcp", Headers: map[string]string{"Authorization": "Bearer ${vault:kv/data/a#tok}"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Remote.Headers["Authorization"]; got != "Bearer ${vault:kv/data/a#tok}" {
		t.Errorf("header = %q, want the reference kept for the dial to resolve", got)
	}
}
