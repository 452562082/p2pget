package engine

import (
	"errors"
	"net/http"
	"testing"
)

// failTransport makes every HTTP request fail, simulating DoH being blocked.
type failTransport struct{}

func (failTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("doh unreachable")
}

func TestFallbackNodesValid(t *testing.T) {
	addrs := fallbackNodes()
	if len(addrs) != len(bootstrapFallbackIPs) {
		t.Fatalf("fallbackNodes returned %d addrs, want %d", len(addrs), len(bootstrapFallbackIPs))
	}
	for i, a := range addrs {
		if a.IP() == nil {
			t.Errorf("addr %d (%s) has nil IP", i, a.String())
		}
		if a.Port() == 0 {
			t.Errorf("addr %d (%s) has zero port", i, a.String())
		}
	}
}

func TestResolveBootstrapNodesFallsBackWhenDoHFails(t *testing.T) {
	client := &http.Client{Transport: failTransport{}}
	addrs := resolveBootstrapNodes(client)
	if len(addrs) != len(bootstrapFallbackIPs) {
		t.Fatalf("resolveBootstrapNodes with failing DoH returned %d addrs, want %d (the fallback set)",
			len(addrs), len(bootstrapFallbackIPs))
	}
}
