package config

import (
	"strings"
	"testing"
)

func TestDecodeDropInServersOnly(t *testing.T) {
	specs, err := decodeDropIn([]byte("servers:\n  - {name: a, type: remote, remote: {url: https://x/}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || specs[0].Name != "a" {
		t.Fatalf("specs = %+v, want one server named a", specs)
	}
	if got := specs[0].Remote.Transport; got != "" {
		t.Fatalf("Remote.Transport = %q, want empty (no defaults applied)", got)
	}
}

func TestDecodeDropInRejectsGlobalKey(t *testing.T) {
	_, err := decodeDropIn([]byte("listen: :1\nservers: []\n"))
	if err == nil {
		t.Fatal("want error for a global key in a drop-in")
	}
	if !strings.Contains(err.Error(), "listen") {
		t.Fatalf("error %q does not name the key listen", err)
	}
}

func TestDecodeDropInEmptyServers(t *testing.T) {
	specs, err := decodeDropIn([]byte("servers: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 0 {
		t.Fatalf("specs = %+v, want empty", specs)
	}
}

func TestParseStillAppliesDefaultsAndExpands(t *testing.T) {
	data := []byte("servers:\n  - name: a\n    type: remote\n    remote:\n      url: https://x/\n      headers: {A: \"${X}\"}\n")
	lookup := func(k string) (string, bool) { return "1", k == "X" }
	c, err := Parse(data, lookup)
	if err != nil {
		t.Fatal(err)
	}
	r := c.Servers[0].Remote
	if r.Transport != "auto" {
		t.Fatalf("Transport = %q, want auto", r.Transport)
	}
	if r.Headers["A"] != "1" {
		t.Fatalf("header A = %q, want 1", r.Headers["A"])
	}
}
