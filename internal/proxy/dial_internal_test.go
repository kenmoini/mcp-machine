package proxy

import (
	"net/http"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHeaderRoundTripperDoesNotMutateRequest(t *testing.T) {
	var got string
	rt := headerRoundTripper{
		base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			got = r.Header.Get("X-A")
			return &http.Response{StatusCode: 200, Body: http.NoBody, Request: r}, nil
		}),
		headers: map[string]string{"X-A": "1"},
	}
	req, _ := http.NewRequest(http.MethodGet, "http://example.invalid/", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got != "1" {
		t.Fatalf("downstream header = %q", got)
	}
	if req.Header.Get("X-A") != "" {
		t.Fatal("original request was mutated")
	}
}
