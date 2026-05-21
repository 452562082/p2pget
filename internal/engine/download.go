package engine

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
)

type Download struct {
	t     *torrent.Torrent
	added time.Time
	// magnet is the URI this download was added from, when applicable.
	// Set once at construction and never mutated, so it needs no lock.
	magnet string
	// skip holds file indices to exclude, carried from persisted state and
	// applied once metadata arrives. Set at construction, read-only after.
	skip []int

	mu  sync.RWMutex
	err error
	// wanted is the per-file selection (the source of truth). It is nil
	// until metadata arrives, then replaced wholesale on every change so
	// readers can snapshot it lock-free. Effective file priority is the
	// projection: file i downloads iff wanted[i] && !paused.
	wanted       []bool
	paused       bool
	lastSampleAt time.Time
	lastDown     int64
	lastUp       int64
	downRate     float64 // bytes/sec, refreshed by SampleRate
	upRate       float64
}

// initSelection builds the file selection once metadata is available, seeding
// it from the restored skip list, then applies it to the torrent.
func (d *Download) initSelection() {
	files := d.t.Files()
	skip := make(map[int]bool, len(d.skip))
	for _, idx := range d.skip {
		skip[idx] = true
	}
	w := make([]bool, len(files))
	for i := range w {
		w[i] = !skip[i]
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.wanted = w
	d.applyLocked()
}

// applyLocked projects (wanted, paused) onto the torrent's file priorities.
// The caller must hold d.mu; doing the read and the torrent writes under one
// lock keeps concurrent pause/select changes from racing to a stale result.
func (d *Download) applyLocked() {
	if d.wanted == nil {
		return // metadata not ready yet
	}
	for i, f := range d.t.Files() {
		if i < len(d.wanted) && d.wanted[i] && !d.paused {
			f.SetPriority(torrent.PiecePriorityNormal)
		} else {
			f.SetPriority(torrent.PiecePriorityNone)
		}
	}
}

// SetPaused pauses or resumes the download. Pausing drops every file to zero
// priority; resuming restores the file selection.
func (d *Download) SetPaused(paused bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.paused = paused
	d.applyLocked()
}

// Paused reports whether the download is currently paused.
func (d *Download) Paused() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.paused
}

// SampleRate refreshes the download/upload rate estimate from the bytes
// transferred since the previous call. Callers should invoke it on a roughly
// regular interval (the TUI and the get command do so once per refresh tick).
func (d *Download) SampleRate() {
	now := time.Now()
	stats := d.t.Stats()
	down := stats.BytesReadData.Int64()
	up := stats.BytesWrittenData.Int64()

	d.mu.Lock()
	defer d.mu.Unlock()
	if elapsed := now.Sub(d.lastSampleAt).Seconds(); !d.lastSampleAt.IsZero() && elapsed > 0 {
		d.downRate = float64(down-d.lastDown) / elapsed
		d.upRate = float64(up-d.lastUp) / elapsed
	}
	d.lastSampleAt = now
	d.lastDown = down
	d.lastUp = up
}

func (d *Download) setErr(err error) {
	d.mu.Lock()
	d.err = err
	d.mu.Unlock()
}

// Err returns the last terminal error on this download (e.g. metadata timeout),
// or nil if still healthy.
func (d *Download) Err() error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.err
}

type Status struct {
	Name        string
	InfoHash    string
	HaveInfo    bool
	TotalBytes  int64
	BytesDone   int64
	Peers       int
	ActivePeers int
	Seeders     int
	Downloaded  int64
	Uploaded    int64
	DownRate    float64 // bytes/sec
	UpRate      float64 // bytes/sec
	Paused      bool
	AddedAt     time.Time
	Err         string
}

func (d *Download) Status() Status {
	t := d.t
	st := Status{
		InfoHash: t.InfoHash().HexString(),
		Name:     t.Name(),
		AddedAt:  d.added,
	}

	stats := t.Stats()
	st.Peers = stats.TotalPeers
	st.ActivePeers = stats.ActivePeers
	st.Seeders = stats.ConnectedSeeders
	st.Downloaded = stats.BytesReadData.Int64()
	st.Uploaded = stats.BytesWrittenData.Int64()

	d.mu.RLock()
	st.Err = errString(d.err)
	st.DownRate = d.downRate
	st.UpRate = d.upRate
	st.Paused = d.paused
	wanted := d.wanted
	d.mu.RUnlock()

	if t.Info() != nil {
		st.HaveInfo = true
		// Count only wanted files so progress reflects the user's file
		// selection. Before initSelection runs, wanted is nil and every
		// file counts (equivalent to a full download).
		for i, f := range t.Files() {
			if i < len(wanted) && !wanted[i] {
				continue
			}
			st.TotalBytes += f.Length()
			st.BytesDone += f.BytesCompleted()
		}
	}
	return st
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ETA estimates time remaining at the current download rate. It returns 0
// when the rate is unknown, the download is complete, or the estimate is so
// large it is effectively meaningless (which also avoids int64 overflow).
func (s Status) ETA() time.Duration {
	if !s.HaveInfo || s.DownRate < 1 || s.BytesDone >= s.TotalBytes {
		return 0
	}
	secs := float64(s.TotalBytes-s.BytesDone) / s.DownRate
	if secs > 99*3600 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

func (d *Download) InfoHash() string { return d.t.InfoHash().HexString() }

// Name returns the torrent name cheaply, without building a full Status.
func (d *Download) Name() string { return d.t.Name() }

// FileInfo describes a single file within a torrent.
type FileInfo struct {
	Index     int
	Path      string
	Length    int64
	Completed int64
	Wanted    bool
}

// Files returns the per-file breakdown, or nil if metadata isn't available yet.
// Wanted reflects the user's selection, independent of pause state.
func (d *Download) Files() []FileInfo {
	if d.t.Info() == nil {
		return nil
	}
	files := d.t.Files()
	d.mu.RLock()
	wanted := d.wanted
	d.mu.RUnlock()
	out := make([]FileInfo, 0, len(files))
	for i, f := range files {
		w := true
		if i < len(wanted) {
			w = wanted[i]
		}
		out = append(out, FileInfo{
			Index:     i,
			Path:      f.DisplayPath(),
			Length:    f.Length(),
			Completed: f.BytesCompleted(),
			Wanted:    w,
		})
	}
	return out
}

// SetFileWanted includes or excludes file #index from the download.
func (d *Download) SetFileWanted(index int, wanted bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.wanted == nil {
		return errors.New("metadata not ready")
	}
	if index < 0 || index >= len(d.wanted) {
		return fmt.Errorf("file index %d out of range [0,%d)", index, len(d.wanted))
	}
	nw := make([]bool, len(d.wanted))
	copy(nw, d.wanted)
	nw[index] = wanted
	d.wanted = nw
	d.applyLocked()
	return nil
}

// SetAllWanted selects or deselects every file at once.
func (d *Download) SetAllWanted(wanted bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.wanted == nil {
		return
	}
	nw := make([]bool, len(d.wanted))
	for i := range nw {
		nw[i] = wanted
	}
	d.wanted = nw
	d.applyLocked()
}

// Progress returns 0..1 once metadata is known, otherwise 0.
func (s Status) Progress() float64 {
	if !s.HaveInfo || s.TotalBytes == 0 {
		return 0
	}
	return float64(s.BytesDone) / float64(s.TotalBytes)
}
