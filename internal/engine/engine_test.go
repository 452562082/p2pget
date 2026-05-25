package engine

import (
	"context"
	"crypto/sha1"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
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

// TestEngineRestartReplacesDownload verifies the stall-recovery primitive:
// Restart drops the old Download, creates a fresh one under the same info-hash,
// and leaves List length unchanged. Pre-metadata case (DHT disabled, no peers
// to fetch from) — exercises the AddTorrentInfoHash fallback path.
func TestEngineRestartReplacesDownload(t *testing.T) {
	eng, err := New(Config{
		DataDir:         t.TempDir(),
		NoDHT:           true,
		NoUpload:        true,
		MetadataTimeout: -1, // disable so awaitMetadata doesn't race us
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Close()

	const hash = "0123456789abcdef0123456789abcdef01234567"
	d1, err := eng.AddInfoHash(hash)
	if err != nil {
		t.Fatalf("AddInfoHash: %v", err)
	}

	if err := eng.Restart(hash); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	list := eng.List()
	if len(list) != 1 {
		t.Fatalf("after Restart: want 1 download, got %d", len(list))
	}
	if list[0] == d1 {
		t.Errorf("Restart did not swap the Download pointer (got same %p)", d1)
	}
	if list[0].InfoHash() != hash {
		t.Errorf("Restart lost the info-hash: got %q want %q", list[0].InfoHash(), hash)
	}
}

// TestEngineRestartUnknownHash makes sure Restart on a hash we don't track
// returns an error instead of silently no-op'ing (which would mask bugs in the
// watchdog).
func TestEngineRestartUnknownHash(t *testing.T) {
	eng, err := New(Config{DataDir: t.TempDir(), NoDHT: true, NoUpload: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Close()

	if err := eng.Restart("0123456789abcdef0123456789abcdef01234567"); err == nil {
		t.Error("Restart on unknown hash returned nil, want error")
	}
}

// TestStoragePieceCompletionSurvivesReopen pins the resume contract: a piece
// marked complete in the persistent DB MUST still read as complete after the
// storage is closed and reopened against the same data dir. anacrolix's
// default (.part files + setCompletionFromPartFiles) silently resets the DB
// on every open for any incomplete file, so without disabling part files the
// engine re-downloads every torrent from scratch on restart.
func TestStoragePieceCompletionSurvivesReopen(t *testing.T) {
	dir := t.TempDir()

	const pieceLen = int64(16 * 1024)
	// Pre-place the file at its final on-disk path. anacrolix's Completion()
	// readback stat()s the file and reports incomplete if it's missing or
	// short, so MarkComplete alone is not enough — the data file has to exist
	// at the right size for completion to register.
	const fileName = "resume_test.bin"
	if err := os.WriteFile(filepath.Join(dir, fileName), make([]byte, pieceLen), 0o644); err != nil {
		t.Fatalf("seed data file: %v", err)
	}
	// The actual piece hash doesn't need to match for MarkComplete to stick —
	// it stamps the DB directly without hashing.
	pieceHash := sha1.Sum(make([]byte, pieceLen))
	info := metainfo.Info{
		Name:        fileName,
		PieceLength: pieceLen,
		Pieces:      pieceHash[:],
		Length:      pieceLen,
	}
	ih := metainfo.HashBytes([]byte("p2pget-resume-test"))
	ctx := context.Background()

	cs := newStorage(dir)
	ts, err := cs.OpenTorrent(ctx, &info, ih)
	if err != nil {
		t.Fatalf("OpenTorrent session 1: %v", err)
	}
	pc := ts.Piece(info.Piece(0))
	if err := pc.MarkComplete(); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}
	if c := pc.Completion(); !c.Complete {
		t.Fatalf("session 1 readback: Complete=%v Ok=%v, want Complete=true", c.Complete, c.Ok)
	}
	if err := cs.Close(); err != nil {
		t.Fatalf("Close session 1: %v", err)
	}

	cs2 := newStorage(dir)
	t.Cleanup(func() { _ = cs2.Close() })
	ts2, err := cs2.OpenTorrent(ctx, &info, ih)
	if err != nil {
		t.Fatalf("OpenTorrent session 2: %v", err)
	}
	pc2 := ts2.Piece(info.Piece(0))
	if c := pc2.Completion(); !c.Complete {
		t.Fatalf("piece 0 completion lost across reopen (Complete=%v Ok=%v) — resume is broken", c.Complete, c.Ok)
	}
}

// TestStorageWritesWithoutPartSuffix locks in the configuration that fixes
// resume. anacrolix's default storage names incomplete files <name>.part and
// rebuilds piece completion at every OpenTorrent based on whether the
// non-.part file exists at the expected size — which destroys all resume
// state for in-progress downloads. We disable part files so writes land at
// the final path and the persistent piece-completion DB is trusted across
// restarts.
func TestStorageWritesWithoutPartSuffix(t *testing.T) {
	dir := t.TempDir()

	const pieceLen = int64(16 * 1024)
	const fileName = "no_part_suffix.bin"
	info := metainfo.Info{
		Name:        fileName,
		PieceLength: pieceLen,
		Pieces:      make([]byte, 20),
		Length:      pieceLen,
	}
	ih := metainfo.HashBytes([]byte("p2pget-nopart-test"))

	cs := newStorage(dir)
	t.Cleanup(func() { _ = cs.Close() })
	ts, err := cs.OpenTorrent(context.Background(), &info, ih)
	if err != nil {
		t.Fatalf("OpenTorrent: %v", err)
	}
	if _, err := ts.Piece(info.Piece(0)).WriteAt([]byte("hello"), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, fileName)); err != nil {
		t.Errorf("expected data written to %q, but stat failed: %v", fileName, err)
	}
	if _, err := os.Stat(filepath.Join(dir, fileName+".part")); err == nil {
		t.Errorf("found %q.part on disk — storage is still using part files, which breaks resume", fileName)
	}
}

// TestMigratePartFiles rescues data left behind by older binaries that used
// anacrolix's default .part-file naming. The current storage writes directly
// to the final path, so .part files would otherwise sit on disk as orphans
// that the engine can neither resume from nor write into.
func TestMigratePartFiles(t *testing.T) {
	dir := t.TempDir()

	must := func(path string, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// (1) Plain leftover .part at top level — should be renamed.
	must(filepath.Join(dir, "movie.mp4.part"), "video")
	// (2) Nested .part inside a torrent subdir — should also be renamed.
	must(filepath.Join(dir, "anime", "ep01.mkv.part"), "anime-ep1")
	// (3) Target already exists — .part must be left alone (don't clobber).
	must(filepath.Join(dir, "song.mp3"), "good-copy")
	must(filepath.Join(dir, "song.mp3.part"), "stale-partial")
	// (4) A file that just happens to end in .part-like text but isn't a
	// .part file — must stay untouched.
	must(filepath.Join(dir, "notes.txt"), "notes")

	migratePartFiles(dir)

	type check struct {
		path        string
		wantExists  bool
		wantContent string
	}
	for _, c := range []check{
		{filepath.Join(dir, "movie.mp4"), true, "video"},
		{filepath.Join(dir, "movie.mp4.part"), false, ""},
		{filepath.Join(dir, "anime", "ep01.mkv"), true, "anime-ep1"},
		{filepath.Join(dir, "anime", "ep01.mkv.part"), false, ""},
		{filepath.Join(dir, "song.mp3"), true, "good-copy"},
		{filepath.Join(dir, "song.mp3.part"), true, "stale-partial"},
		{filepath.Join(dir, "notes.txt"), true, "notes"},
	} {
		data, err := os.ReadFile(c.path)
		if c.wantExists {
			if err != nil {
				t.Errorf("%s: want present, stat err=%v", c.path, err)
				continue
			}
			if string(data) != c.wantContent {
				t.Errorf("%s: content=%q, want %q", c.path, data, c.wantContent)
			}
		} else if err == nil {
			t.Errorf("%s: want gone, but still present", c.path)
		}
	}
}
