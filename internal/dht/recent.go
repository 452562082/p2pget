package dht

import "sync"

// recentSet is a bounded set with two-generation eviction. Lookups and inserts
// are amortized O(1) and memory is capped at ~2 * cap entries. Used by the
// crawler to deduplicate the same info-hash arriving repeatedly from DHT
// traffic without touching SQLite each time.
type recentSet struct {
	mu     sync.Mutex
	active map[string]struct{}
	old    map[string]struct{}
	cap    int
}

func newRecentSet(cap int) *recentSet {
	if cap <= 0 {
		cap = 1024
	}
	return &recentSet{
		active: make(map[string]struct{}, cap),
		cap:    cap,
	}
}

// seen reports whether h was already present, and inserts it otherwise.
func (r *recentSet) seen(h string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.active[h]; ok {
		return true
	}
	if _, ok := r.old[h]; ok {
		r.active[h] = struct{}{}
		return true
	}
	if len(r.active) >= r.cap {
		r.old = r.active
		r.active = make(map[string]struct{}, r.cap)
	}
	r.active[h] = struct{}{}
	return false
}
