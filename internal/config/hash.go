package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Hash returns the lowercase hex sha256 of the canonical JSON form of the
// spec plus the runtime image it will run on. It is stored as a container
// label to decide whether a container must be recreated after a config
// change.
//
// The canonical form is the encoding/json encoding of the spec (fields in
// declared order, map keys sorted) with Enabled cleared, so toggling a
// server on or off never changes its hash, followed by the runtime image as
// a separate field.
func Hash(s ServerSpec, runtimeImage string) string {
	s.Enabled = nil // s is a copy; the caller's spec is untouched.
	b, err := json.Marshal(struct {
		Spec         ServerSpec
		RuntimeImage string
	}{s, runtimeImage})
	if err != nil {
		// ServerSpec contains only strings, ints, slices, maps of strings and
		// pointers to such structs; marshaling cannot fail.
		panic("config: marshal spec for hash: " + err.Error())
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
