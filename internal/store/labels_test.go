package store_test

import (
	"testing"

	"github.com/kenmoini/mcp-machine/internal/store"
)

func TestLabelsRoundTrip(t *testing.T) {
	m := store.Labels("github", "abc123", "stdio")
	if m[store.LabelManaged] != "true" {
		t.Fatalf("managed label = %q, want true", m[store.LabelManaged])
	}
	info, ok := store.ParseLabels(m)
	if !ok {
		t.Fatal("ParseLabels ok = false, want true")
	}
	want := store.LabelInfo{Name: "github", Hash: "abc123", Transport: "stdio"}
	if info != want {
		t.Errorf("info = %+v, want %+v", info, want)
	}
	if got := store.ContainerName("github"); got != "mcpm-github" {
		t.Errorf("ContainerName = %q, want mcpm-github", got)
	}
}

func TestParseLabelsRejectsUnmanaged(t *testing.T) {
	cases := map[string]map[string]string{
		"nil":            nil,
		"empty":          {},
		"not managed":    {store.LabelServer: "x"},
		"managed false":  {store.LabelManaged: "false", store.LabelServer: "x"},
		"no server":      {store.LabelManaged: "true"},
		"empty server":   {store.LabelManaged: "true", store.LabelServer: ""},
		"managed capped": {store.LabelManaged: "True", store.LabelServer: "x"},
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := store.ParseLabels(m); ok {
				t.Errorf("ParseLabels(%v) ok = true, want false", m)
			}
		})
	}
}

func TestParseLabelsAllowsEmptyHashAndTransport(t *testing.T) {
	info, ok := store.ParseLabels(map[string]string{store.LabelManaged: "true", store.LabelServer: "x"})
	if !ok || info.Name != "x" || info.Hash != "" || info.Transport != "" {
		t.Errorf("got %+v ok=%v", info, ok)
	}
}
