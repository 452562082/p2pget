package dht

import (
	"fmt"
	"testing"
)

func TestRecentSetSeen(t *testing.T) {
	r := newRecentSet(8)
	if r.seen("a") {
		t.Error("first seen of a should report false")
	}
	if !r.seen("a") {
		t.Error("second seen of a should report true")
	}
	if r.seen("b") {
		t.Error("first seen of b should report false")
	}
}

func TestRecentSetRemembersAcrossGeneration(t *testing.T) {
	r := newRecentSet(2)
	r.seen("a")
	r.seen("b") // active now {a,b}, at capacity
	r.seen("c") // triggers rotation: old={a,b}, active={c}
	if !r.seen("a") {
		t.Error("a should still be remembered via the old generation")
	}
}

func TestRecentSetBounded(t *testing.T) {
	r := newRecentSet(10)
	for i := 0; i < 1000; i++ {
		r.seen(fmt.Sprintf("hash-%d", i))
	}
	if n := len(r.active) + len(r.old); n > 20 {
		t.Errorf("recentSet grew to %d entries, want <= 2*cap (20)", n)
	}
}
