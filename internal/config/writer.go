package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Writer edits the servers list of a config file in place. It works on the
// YAML node tree, so comments, key order, quoting and ${VAR} placeholders in
// the rest of the file survive an edit. Each edit is validated before it
// touches disk and is written atomically (temp file plus rename).
type Writer struct{ Path string }

// AddServer appends s to the servers list. It is an error if a server with the
// same name already exists.
func (w Writer) AddServer(s ServerSpec) error {
	return w.edit(func(seq *yaml.Node) error {
		if findServer(seq, s.Name) >= 0 {
			return fmt.Errorf("config: server %q already exists", s.Name)
		}
		n, err := specNode(s)
		if err != nil {
			return err
		}
		seq.Content = append(seq.Content, n)
		return nil
	})
}

// UpdateServer merges s into the existing entry for name, keeping its position
// in the list, its key order and every comment and quoting style of values
// that did not change (see mergeMapping). It is an error if no such server
// exists, or if s.Name collides with a different server.
func (w Writer) UpdateServer(name string, s ServerSpec) error {
	return w.edit(func(seq *yaml.Node) error {
		i := findServer(seq, name)
		if i < 0 {
			return notFound(name)
		}
		if s.Name != name && findServer(seq, s.Name) >= 0 {
			return fmt.Errorf("config: server %q already exists", s.Name)
		}
		n, err := specNode(s)
		if err != nil {
			return err
		}
		mergeMapping(seq.Content[i], n)
		return nil
	})
}

// DeleteServer removes the server called name. It is an error if no such
// server exists.
func (w Writer) DeleteServer(name string) error {
	return w.edit(func(seq *yaml.Node) error {
		i := findServer(seq, name)
		if i < 0 {
			return notFound(name)
		}
		seq.Content = append(seq.Content[:i], seq.Content[i+1:]...)
		return nil
	})
}

func notFound(name string) error { return fmt.Errorf("config: server %q not found", name) }

// acceptAll resolves every variable so ${VAR} text is never required or
// resolved while validating an edit.
func acceptAll(string) (string, bool) { return "x", true }

// edit reads the file, applies fn to its servers sequence, validates the
// result and atomically replaces the file.
func (w Writer) edit(fn func(seq *yaml.Node) error) error {
	fi, err := os.Stat(w.Path)
	if err != nil {
		return fmt.Errorf("config: stat %s: %w", w.Path, err)
	}
	data, err := os.ReadFile(w.Path)
	if err != nil {
		return fmt.Errorf("config: read %s: %w", w.Path, err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("config: parse: %w", err)
	}
	seq, err := serversNode(&root)
	if err != nil {
		return err
	}
	if err := fn(seq); err != nil {
		return err
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return fmt.Errorf("config: encode: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("config: encode: %w", err)
	}
	out := buf.Bytes()

	c, err := Parse(out, acceptAll)
	if err != nil {
		return err
	}
	if err := Validate(c); err != nil {
		return err
	}
	return writeAtomic(w.Path, out, fi.Mode().Perm())
}

// serversNode returns the sequence node under the top-level "servers" key,
// creating the key (or converting an empty/flow value) as needed.
func serversNode(root *yaml.Node) (*yaml.Node, error) {
	if root.Kind == 0 { // empty document
		root.Kind = yaml.DocumentNode
		root.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("config: top level must be a mapping")
	}
	m := root.Content[0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != "servers" {
			continue
		}
		v := m.Content[i+1]
		switch {
		case v.Kind == yaml.SequenceNode:
		case v.Kind == yaml.ScalarNode && v.Tag == "!!null":
			*v = yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		default:
			return nil, errors.New("config: servers must be a list")
		}
		// New entries are block mappings; a flow list ("[]") would force
		// them inline.
		v.Style &^= yaml.FlowStyle
		return v, nil
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "servers"}, seq)
	return seq, nil
}

// findServer returns the index of the entry whose name is name, or -1.
func findServer(seq *yaml.Node, name string) int {
	for i, e := range seq.Content {
		if e.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(e.Content); j += 2 {
			if e.Content[j].Value == "name" && e.Content[j+1].Value == name {
				return i
			}
		}
	}
	return -1
}

// mergeMapping merges nw into old in place. Keys present in both keep the old
// key node (and its comments) and have their values merged; keys only in old
// are removed (omitempty means the field is now empty); keys only in nw are
// appended in nw's order.
func mergeMapping(old, nw *yaml.Node) {
	newIdx := make(map[string]int, len(nw.Content)/2)
	for j := 0; j+1 < len(nw.Content); j += 2 {
		newIdx[nw.Content[j].Value] = j
	}
	merged := make([]*yaml.Node, 0, len(nw.Content))
	seen := make(map[string]bool, len(newIdx))
	for i := 0; i+1 < len(old.Content); i += 2 {
		k := old.Content[i]
		j, ok := newIdx[k.Value]
		if !ok || seen[k.Value] {
			continue
		}
		seen[k.Value] = true
		merged = append(merged, k, mergeValue(old.Content[i+1], nw.Content[j+1]))
	}
	for j := 0; j+1 < len(nw.Content); j += 2 {
		if !seen[nw.Content[j].Value] {
			merged = append(merged, nw.Content[j], nw.Content[j+1])
		}
	}
	old.Content = merged
}

// mergeValue returns the node to use in place of old given the freshly
// encoded nw. Mappings merge recursively; an unchanged scalar or sequence
// keeps the old node untouched (preserving quoting and flow style); a changed
// one is replaced by nw carrying over the old comments (and, for sequences,
// the old flow style).
func mergeValue(old, nw *yaml.Node) *yaml.Node {
	switch {
	case old.Kind == yaml.MappingNode && nw.Kind == yaml.MappingNode:
		mergeMapping(old, nw)
		return old
	case old.Kind == yaml.ScalarNode && nw.Kind == yaml.ScalarNode:
		if old.Value == nw.Value && old.Tag == nw.Tag {
			return old
		}
	case old.Kind == yaml.SequenceNode && nw.Kind == yaml.SequenceNode:
		if sameScalars(old, nw) {
			return old
		}
		nw.Style |= old.Style & yaml.FlowStyle
	}
	nw.HeadComment, nw.LineComment, nw.FootComment = old.HeadComment, old.LineComment, old.FootComment
	return nw
}

// sameScalars reports whether a and b are sequences of equal scalars.
func sameScalars(a, b *yaml.Node) bool {
	if len(a.Content) != len(b.Content) {
		return false
	}
	for i := range a.Content {
		x, y := a.Content[i], b.Content[i]
		if x.Kind != yaml.ScalarNode || y.Kind != yaml.ScalarNode || x.Value != y.Value || x.Tag != y.Tag {
			return false
		}
	}
	return true
}

// specNode renders s as a block mapping node. Nil sections and empty fields
// are omitted by the omitempty tags on ServerSpec.
func specNode(s ServerSpec) (*yaml.Node, error) {
	b, err := yaml.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("config: encode server %q: %w", s.Name, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("config: encode server %q: %w", s.Name, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, fmt.Errorf("config: encode server %q: unexpected document", s.Name)
	}
	return doc.Content[0], nil
}

// writeAtomic writes data to path+".tmp" with the given mode and renames it
// over path. The temp file is removed on failure.
func writeAtomic(path string, data []byte, mode os.FileMode) (err error) {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("config: write %s: %w", tmp, err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("config: write %s: %w", tmp, err)
	}
	if err = f.Chmod(mode); err != nil {
		_ = f.Close()
		return fmt.Errorf("config: chmod %s: %w", tmp, err)
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("config: sync %s: %w", tmp, err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("config: close %s: %w", tmp, err)
	}
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("config: rename %s: %w", tmp, err)
	}
	return nil
}
