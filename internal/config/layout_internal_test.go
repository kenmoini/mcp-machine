package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestListDropInsFilters(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"b.yml", "a.yaml", ".hidden.yaml", "c.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "d.yaml"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "dir.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "e.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "nowhere"), filepath.Join(dir, "broken.yaml")); err != nil {
		t.Fatal(err)
	}

	got, err := listDropIns(dir)
	if err != nil {
		t.Fatalf("listDropIns: %v", err)
	}
	want := []string{filepath.Join(dir, "a.yaml"), filepath.Join(dir, "b.yml"), filepath.Join(dir, "e.yaml")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("listDropIns = %v, want %v", got, want)
	}
}

func TestListDropInsMissingDir(t *testing.T) {
	got, err := listDropIns(filepath.Join(t.TempDir(), "absent"))
	if err != nil || got != nil {
		t.Errorf("listDropIns(missing) = %v, %v; want nil, nil", got, err)
	}
}
