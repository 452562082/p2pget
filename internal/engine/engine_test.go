package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestDHTStatsEmptyWhenDisabled checks DHTStats returns no entries when DHT
// is disabled — guarding against accidentally exposing stale or nil-deref'd
// server objects when NoDHT skips DHT setup entirely.
func TestDHTStatsEmptyWhenDisabled(t *testing.T) {
	eng, err := New(Config{DataDir: t.TempDir(), NoDHT: true, NoUpload: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Close()
	if got := eng.DHTStats(); len(got) != 0 {
		t.Errorf("DHTStats with NoDHT=true returned %d entries, want 0", len(got))
	}
}

// TestWaitInfoReturnsOnMetadataTimeout ensures WaitInfo unblocks when a
// download is dropped after its metadata fetch times out. GotInfo() never
// fires once the torrent is dropped, so a WaitInfo that only watches GotInfo()
// would hang until the caller's context expires.
func TestWaitInfoReturnsOnMetadataTimeout(t *testing.T) {
	eng, err := New(Config{
		DataDir:         t.TempDir(),
		NoDHT:           true,
		NoUpload:        true,
		MetadataTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Close()

	// An arbitrary info-hash with DHT disabled has no way to ever fetch
	// metadata, so awaitMetadata times it out and drops it.
	d, err := eng.AddInfoHash("0123456789abcdef0123456789abcdef01234567")
	if err != nil {
		t.Fatalf("AddInfoHash: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	err = eng.WaitInfo(ctx, d)
	elapsed := time.Since(start)

	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitInfo blocked until the context deadline (%v) — it ignored the dropped torrent", elapsed)
	}
	if err == nil {
		t.Fatal("WaitInfo returned nil — metadata cannot have arrived with DHT disabled")
	}
	if elapsed > 2*time.Second {
		t.Errorf("WaitInfo took %v to notice the timeout; expected well under the 5s context", elapsed)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("WaitInfo error = %q, want it to mention the metadata timeout", err)
	}
}
