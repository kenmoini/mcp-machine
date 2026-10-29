package vault

import (
	"context"
	"net/http"
	"sync"
)

// Fake is an in-memory Reader for tests: Secrets maps a path to its data.
// Reads of a missing path answer a 404 ReadError; Err, when set, fails every
// read.
type Fake struct {
	mu      sync.Mutex
	Secrets map[string]map[string]any
	Err     error
	reads   []string
}

var _ Reader = (*Fake)(nil)

// Read returns the fake's data for path.
func (f *Fake) Read(_ context.Context, path string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, path)
	if f.Err != nil {
		return nil, f.Err
	}
	data, ok := f.Secrets[path]
	if !ok {
		return nil, &ReadError{Path: path, Status: http.StatusNotFound, Msg: "not found"}
	}
	out := make(map[string]any, len(data))
	for k, v := range data {
		out[k] = v
	}
	return out, nil
}

// Set stores one key of a secret, creating the secret if needed.
func (f *Fake) Set(path, key string, value any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Secrets == nil {
		f.Secrets = map[string]map[string]any{}
	}
	if f.Secrets[path] == nil {
		f.Secrets[path] = map[string]any{}
	}
	f.Secrets[path][key] = value
}

// Fail makes every following read return err (nil restores reads).
func (f *Fake) Fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Err = err
}

// Reads returns the paths read so far, in order.
func (f *Fake) Reads() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reads...)
}
