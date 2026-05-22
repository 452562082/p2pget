package engine

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
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

// dohTransport serves a canned DoH JSON answer to every request.
type dohTransport struct{ json string }

func (d dohTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(d.json)),
		Header:     make(http.Header),
	}, nil
}

func TestResolveBootstrapNodesUsesDoHAnswers(t *testing.T) {
	// Every bootstrap host resolves, via the stubbed DoH endpoint, to one
	// documentation IP (RFC 5737 TEST-NET-3).
	const answer = `{"Answer":[{"type":1,"data":"203.0.113.7"}]}`
	client := &http.Client{Transport: dohTransport{json: answer}}

	addrs := resolveBootstrapNodes(client)

	// One addr per bootstrap host — distinct from the 6-entry hardcoded
	// fallback, which proves the DoH path ran rather than the fallback.
	if len(addrs) != len(bootstrapHostPorts) {
		t.Fatalf("got %d addrs, want %d (one per bootstrap host) — DoH path may not have run",
			len(addrs), len(bootstrapHostPorts))
	}
	want := net.ParseIP("203.0.113.7")
	for i, a := range addrs {
		if !a.IP().Equal(want) {
			t.Errorf("addr %d IP = %s, want 203.0.113.7 (from the DoH answer)", i, a.IP())
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
	for i, a := range addrs {
		if a.IP() == nil {
			t.Errorf("addr %d (%s) has nil IP", i, a.String())
		}
		if a.Port() == 0 {
			t.Errorf("addr %d (%s) has zero port", i, a.String())
		}
	}
}
