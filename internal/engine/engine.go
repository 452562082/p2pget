// Package engine wraps anacrolix/torrent and exposes a small, opinionated API.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/dht/v2"
	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"golang.org/x/time/rate"
)

type Engine struct {
	client          *torrent.Client
	dataDir         string
	metadataTimeout time.Duration

	statePath string
	stateMu   sync.Mutex

	mu        sync.RWMutex
	downloads map[metainfo.Hash]*Download
}

type Config struct {
	DataDir    string
	ListenPort int
	NoUpload   bool
	NoDHT      bool
	// MetadataTimeout bounds how long a newly-added torrent will wait for
	// metadata before being dropped and marked as failed. 0 means default
	// (10 minutes). Set to a negative value to disable the timeout.
	MetadataTimeout time.Duration
	// StatePath, if non-empty, is a JSON file the engine uses to persist the
	// download list and restore it on the next start. Empty disables both.
	StatePath string
	// DownloadRate / UploadRate cap throughput in bytes/sec. 0 means unlimited.
	DownloadRate int
	UploadRate   int
	// MaxConns caps established peer connections per torrent. 0 uses the
	// anacrolix/torrent default.
	MaxConns int
}

func New(cfg Config) (*Engine, error) {
	if cfg.DataDir == "" {
		home, _ := os.UserHomeDir()
		cfg.DataDir = filepath.Join(home, ".p2pget", "data")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir data dir: %w", err)
	}
	// Rescue any "<name>.part" files left behind by older binaries that ran
	// with anacrolix's default part-file naming. Without this they'd sit on
	// disk as orphans next to whatever the no-part-file storage writes today.
	migratePartFiles(cfg.DataDir)

	tcfg := torrent.NewDefaultClientConfig()
	tcfg.DataDir = cfg.DataDir
	tcfg.DefaultStorage = newStorage(cfg.DataDir)
	tcfg.Seed = !cfg.NoUpload
	tcfg.NoDHT = cfg.NoDHT
	// Bootstrap the DHT via DoH-resolved / hardcoded node addresses instead of
	// the system DNS, which some networks hijack for BitTorrent domains.
	tcfg.DhtStartingNodes = func(string) dht.StartingNodesGetter {
		return func() ([]dht.Addr, error) { return bootstrapNodes(), nil }
	}
	// Port 0 means ":0" — let the OS assign a free port. Otherwise the
	// listen address is left unset and anacrolix falls back to a fixed
	// default (42069), which collides when a second instance starts.
	if cfg.ListenPort > 0 {
		tcfg.SetListenAddr(fmt.Sprintf(":%d", cfg.ListenPort))
	} else {
		tcfg.SetListenAddr(":0")
	}
	if cfg.DownloadRate > 0 {
		tcfg.DownloadRateLimiter = rate.NewLimiter(rate.Limit(cfg.DownloadRate), rateBurst(cfg.DownloadRate))
	}
	if cfg.UploadRate > 0 {
		tcfg.UploadRateLimiter = rate.NewLimiter(rate.Limit(cfg.UploadRate), rateBurst(cfg.UploadRate))
	}
	if cfg.MaxConns > 0 {
		tcfg.EstablishedConnsPerTorrent = cfg.MaxConns
	}

	cli, err := torrent.NewClient(tcfg)
	if err != nil {
		return nil, fmt.Errorf("torrent client: %w", err)
	}

	timeout := cfg.MetadataTimeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}

	e := &Engine{
		client:          cli,
		dataDir:         cfg.DataDir,
		metadataTimeout: timeout,
		statePath:       cfg.StatePath,
		downloads:       map[metainfo.Hash]*Download{},
	}
	if e.statePath != "" {
		if err := os.MkdirAll(filepath.Dir(e.statePath), 0o755); err != nil {
			return nil, fmt.Errorf("mkdir state dir: %w", err)
		}
		e.restore()
	}
	return e, nil
}

// newStorage builds the file-backed storage shared by every torrent in this
// engine. anacrolix's default (storage.NewFile) writes incomplete files with a
// .part suffix and, on every OpenTorrent, calls setCompletionFromPartFiles
// which OVERWRITES the persistent piece-completion DB to mark every piece in
// any .part file as incomplete — torching resume state for any in-progress
// download. Disabling part files keeps the persistent sqlite DB authoritative
// across restarts, which is what users actually expect.
func newStorage(dataDir string) storage.ClientImplCloser {
	return storage.NewFileOpts(storage.NewFileClientOpts{
		ClientBaseDir: dataDir,
		UsePartFiles:  g.Some(false),
	})
}

// migratePartFiles strips the ".part" suffix from any leftover incomplete
// files in dataDir. Older binaries (and anacrolix by default) named partially
// downloaded files "<name>.part" and renamed them on completion; we now write
// directly to the final path, so any pre-existing .part files would otherwise
// be invisible to anacrolix. Files whose un-suffixed target already exists are
// left alone — those are either complete (don't overwrite the good copy) or
// from an inconsistent half-rename we shouldn't second-guess.
func migratePartFiles(dataDir string) {
	_ = filepath.WalkDir(dataDir, func(path string, de fs.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".part") {
			return nil
		}
		target := strings.TrimSuffix(path, ".part")
		if _, err := os.Stat(target); err == nil {
			return nil // don't clobber an existing file
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil // unexpected error; leave the .part alone
		}
		_ = os.Rename(path, target)
		return nil
	})
}

// rateBurst picks a token-bucket burst for a byte/sec rate. anacrolix requires
// the burst to comfortably exceed its 16 KiB chunk size; allowing ~1s of data
// (floored at 256 KiB) keeps throughput smooth without spiky overshoot.
func rateBurst(bytesPerSec int) int {
	const minBurst = 256 << 10
	if bytesPerSec < minBurst {
		return minBurst
	}
	return bytesPerSec
}

func (e *Engine) DataDir() string { return e.dataDir }

func (e *Engine) Close() error {
	// Persist while torrents are still queryable, so file-selection changes
	// made during the session are captured before shutdown.
	e.persistState()
	errs := e.client.Close()
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

// AddMagnet adds a magnet URI and starts downloading once metadata arrives.
func (e *Engine) AddMagnet(uri string) (*Download, error) {
	t, err := e.client.AddMagnet(uri)
	if err != nil {
		return nil, err
	}
	d := e.track(t, uri, nil)
	e.persistState()
	return d, nil
}

// AddInfoHash adds a torrent by 40-char hex info-hash and waits on DHT/peers for metadata.
func (e *Engine) AddInfoHash(hex string) (*Download, error) {
	hex = strings.ToLower(strings.TrimSpace(hex))
	if len(hex) != 40 {
		return nil, fmt.Errorf("info-hash must be 40 hex chars, got %d", len(hex))
	}
	var h metainfo.Hash
	if err := h.FromHexString(hex); err != nil {
		return nil, err
	}
	t, _ := e.client.AddTorrentInfoHash(h)
	d := e.track(t, "", nil)
	e.persistState()
	return d, nil
}

// AddTorrentFile adds a local .torrent file.
func (e *Engine) AddTorrentFile(path string) (*Download, error) {
	mi, err := metainfo.LoadFromFile(path)
	if err != nil {
		return nil, err
	}
	t, err := e.client.AddTorrent(mi)
	if err != nil {
		return nil, err
	}
	d := e.track(t, "", nil)
	e.persistState()
	return d, nil
}

// AddTorrentURL fetches a .torrent file over HTTP(S) and adds it.
func (e *Engine) AddTorrentURL(rawurl string) (*Download, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch torrent: HTTP %d", resp.StatusCode)
	}
	mi, err := metainfo.Load(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("parse torrent: %w", err)
	}
	t, err := e.client.AddTorrent(mi)
	if err != nil {
		return nil, err
	}
	d := e.track(t, "", nil)
	e.persistState()
	return d, nil
}

func (e *Engine) track(t *torrent.Torrent, magnet string, skip []int) *Download {
	hash := t.InfoHash()
	e.mu.Lock()
	// Reuse a healthy existing download, but replace one that already
	// failed (e.g. a metadata timeout) so re-adding it actually retries
	// instead of handing back a dropped, dead torrent.
	if d, ok := e.downloads[hash]; ok && d.Err() == nil {
		e.mu.Unlock()
		return d
	}
	d := &Download{t: t, added: time.Now(), magnet: magnet, skip: skip}
	e.downloads[hash] = d
	e.mu.Unlock()

	// Once metadata is ready, start the wanted files. If metadata never
	// arrives, fail the download cleanly so the UI can show why.
	go e.awaitMetadata(d)

	return d
}

func (e *Engine) awaitMetadata(d *Download) {
	t := d.t
	if e.metadataTimeout < 0 {
		select {
		case <-t.GotInfo():
			e.recoverExistingData(t)
			d.initSelection()
		case <-t.Closed():
		}
		return
	}
	timer := time.NewTimer(e.metadataTimeout)
	defer timer.Stop()
	select {
	case <-t.GotInfo():
		e.recoverExistingData(t)
		d.initSelection()
	case <-t.Closed():
		// removed before metadata arrived; nothing to do
	case <-timer.C:
		d.setErr(fmt.Errorf("metadata fetch timed out after %s", e.metadataTimeout))
		// Drop the torrent so it stops burning peer slots and bandwidth,
		// but keep the Download entry so the UI can surface the failure
		// until the user removes it explicitly.
		t.Drop()
	}
}

// recoverExistingData hash-checks any pre-existing on-disk content for the
// torrent so partial files left behind by older binaries (or by an interrupted
// session before the persistent piece-completion DB has been seeded) are
// credited instead of silently re-downloaded. It runs synchronously before
// initSelection so the verify happens before any download priority is set —
// otherwise anacrolix could overwrite valid bytes mid-hash. It is a no-op
// when the DB already knows about completed pieces or no data files exist.
func (e *Engine) recoverExistingData(t *torrent.Torrent) {
	if t.BytesCompleted() > 0 {
		return // DB already has truth for this torrent
	}
	info := t.Info()
	if info == nil {
		return
	}
	base := filepath.Join(e.dataDir, info.BestName())
	fi, err := os.Stat(base)
	if err != nil {
		return // no leftover data
	}
	// For single-file torrents BestName() IS the file. For multi-file
	// torrents it's the parent dir — either way, presence means there is
	// something to verify.
	if !fi.IsDir() && fi.Size() == 0 {
		return
	}
	_ = t.VerifyDataContext(context.Background())
}

// List returns a snapshot of all known downloads, ordered by when they were
// added so the UI presents a stable list across refreshes.
func (e *Engine) List() []*Download {
	e.mu.RLock()
	out := make([]*Download, 0, len(e.downloads))
	for _, d := range e.downloads {
		out = append(out, d)
	}
	e.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].added.Before(out[j].added) })
	return out
}

// SampleRates refreshes the transfer-rate estimate of every download. Call it
// on a regular interval so Status reports meaningful speeds.
func (e *Engine) SampleRates() {
	for _, d := range e.List() {
		d.SampleRate()
	}
}

// Remove drops a download. If deleteData is true, also wipes downloaded files.
func (e *Engine) Remove(hash string, deleteData bool) error {
	var h metainfo.Hash
	if err := h.FromHexString(hash); err != nil {
		return err
	}
	e.mu.Lock()
	d, ok := e.downloads[h]
	if ok {
		delete(e.downloads, h)
	}
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("no such download: %s", hash)
	}
	d.t.Drop()
	if deleteData {
		_ = os.RemoveAll(filepath.Join(e.dataDir, d.t.Name()))
	}
	e.persistState()
	return nil
}

type taskRecord struct {
	InfoHash string `json:"info_hash"`
	Magnet   string `json:"magnet,omitempty"`
	// Skip lists deselected file indices, so a partial file selection
	// survives a restart instead of reverting to a full download.
	Skip []int `json:"skip,omitempty"`
}

// persistState writes the current download list to statePath atomically.
// It is a no-op when persistence is disabled.
func (e *Engine) persistState() {
	if e.statePath == "" {
		return
	}
	e.stateMu.Lock()
	defer e.stateMu.Unlock()

	e.mu.RLock()
	dls := make([]*Download, 0, len(e.downloads))
	for _, d := range e.downloads {
		dls = append(dls, d)
	}
	e.mu.RUnlock()

	recs := make([]taskRecord, 0, len(dls))
	for _, d := range dls {
		rec := taskRecord{InfoHash: d.InfoHash(), Magnet: d.magnet}
		for _, f := range d.Files() {
			if !f.Wanted {
				rec.Skip = append(rec.Skip, f.Index)
			}
		}
		recs = append(recs, rec)
	}

	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return
	}
	tmp := e.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, e.statePath)
}

// restore re-adds downloads recorded in statePath. anacrolix/torrent picks up
// any already-downloaded data in the data dir, so this effectively resumes.
// It adds via track directly so persistState is not re-triggered mid-restore.
func (e *Engine) restore() {
	data, err := os.ReadFile(e.statePath)
	if err != nil {
		return // no prior state
	}
	var recs []taskRecord
	if err := json.Unmarshal(data, &recs); err != nil {
		return
	}
	for _, r := range recs {
		switch {
		case r.Magnet != "":
			t, err := e.client.AddMagnet(r.Magnet)
			if err != nil {
				continue
			}
			e.track(t, r.Magnet, r.Skip)
		case len(r.InfoHash) == 40:
			var h metainfo.Hash
			if err := h.FromHexString(strings.ToLower(r.InfoHash)); err != nil {
				continue
			}
			t, _ := e.client.AddTorrentInfoHash(h)
			e.track(t, "", r.Skip)
		}
	}
}

// WaitInfo blocks until torrent metadata is available or ctx is done. It also
// returns if the torrent is dropped first — e.g. when awaitMetadata gives up
// after the metadata timeout — since GotInfo() never fires for a dropped
// torrent and would otherwise hang the caller until ctx expires.
func (e *Engine) WaitInfo(ctx context.Context, d *Download) error {
	select {
	case <-d.t.GotInfo():
		return nil
	case <-d.t.Closed():
		if err := d.Err(); err != nil {
			return err
		}
		return errors.New("torrent closed before metadata arrived")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// FetchInfo adds an info-hash, waits for metadata via BEP9, returns it,
// and drops the torrent without downloading content. Used by the DHT crawler.
func (e *Engine) FetchInfo(ctx context.Context, hex string) (*metainfo.Info, error) {
	hex = strings.ToLower(strings.TrimSpace(hex))
	var h metainfo.Hash
	if err := h.FromHexString(hex); err != nil {
		return nil, err
	}
	t, _ := e.client.AddTorrentInfoHash(h)
	defer t.Drop()
	select {
	case <-t.GotInfo():
		return t.Info(), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Client exposes the underlying torrent client for advanced use (DHT crawler).
func (e *Engine) Client() *torrent.Client { return e.client }

// DHTStats describes one DHT server's routing-table state, decoupled from the
// anacrolix concrete types so callers needn't import the DHT package.
type DHTStats struct {
	Addr       string
	Nodes      int
	Good       int
	Bad        uint
	OutQueries int64
}

// DHTStats returns a snapshot of each DHT server's routing table. The slice is
// empty when DHT is disabled (NoDHT) or when no server type matches.
func (e *Engine) DHTStats() []DHTStats {
	var out []DHTStats
	for _, s := range e.client.DhtServers() {
		st, ok := s.Stats().(dht.ServerStats)
		if !ok {
			continue
		}
		out = append(out, DHTStats{
			Addr:       s.Addr().String(),
			Nodes:      st.Nodes,
			Good:       st.GoodNodes,
			Bad:        st.BadNodes,
			OutQueries: st.OutboundQueriesAttempted,
		})
	}
	return out
}
