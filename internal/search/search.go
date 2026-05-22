// Package search aggregates BitTorrent indexers behind a single interface.
package search

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

type Result struct {
	Title    string
	InfoHash string
	Magnet   string
	Size     int64
	Seeders  int
	Leechers int
	Source   string
	Date     time.Time
}

type Indexer interface {
	Name() string
	Search(ctx context.Context, query string) ([]Result, error)
}

type Aggregator struct {
	Indexers []Indexer
	Timeout  time.Duration
}

func New(idx ...Indexer) *Aggregator {
	return &Aggregator{Indexers: idx, Timeout: 15 * time.Second}
}

func (a *Aggregator) Search(ctx context.Context, query string) ([]Result, map[string]error) {
	if a.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.Timeout)
		defer cancel()
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		out  []Result
		errs = map[string]error{}
	)

	for _, idx := range a.Indexers {
		wg.Add(1)
		go func(idx Indexer) {
			defer wg.Done()
			res, err := idx.Search(ctx, query)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[idx.Name()] = err
				return
			}
			out = append(out, filterByQuery(res, query)...)
		}(idx)
	}
	wg.Wait()

	out = DedupAndRank(out)
	return out, errs
}

// IndexerResult is one indexer's contribution, delivered by SearchStream.
type IndexerResult struct {
	Name    string
	Results []Result
	Err     error
}

// SearchStream queries every indexer concurrently and returns a channel that
// yields each indexer's result as soon as it finishes, then closes. The
// caller does its own dedup/rank on the accumulated results. The channel is
// buffered so producer goroutines never block, even if the caller stops
// reading early.
func (a *Aggregator) SearchStream(ctx context.Context, query string) <-chan IndexerResult {
	cancel := context.CancelFunc(func() {})
	if a.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, a.Timeout)
	}

	ch := make(chan IndexerResult, len(a.Indexers))
	var wg sync.WaitGroup
	for _, idx := range a.Indexers {
		wg.Add(1)
		go func(idx Indexer) {
			defer wg.Done()
			res, err := idx.Search(ctx, query)
			ch <- IndexerResult{Name: idx.Name(), Results: filterByQuery(res, query), Err: err}
		}(idx)
	}
	go func() {
		wg.Wait()
		cancel()
		close(ch)
	}()
	return ch
}

// filterByQuery drops results whose title doesn't contain every
// whitespace-separated token of the query (case-insensitive). Some sources
// (notably apibay) return a generic popular list when a query matches
// nothing — e.g. a Chinese term against an English-only index — and this
// removes that irrelevant noise.
func filterByQuery(in []Result, query string) []Result {
	tokens := strings.Fields(strings.ToLower(query))
	if len(tokens) == 0 {
		return in
	}
	out := make([]Result, 0, len(in))
	for _, r := range in {
		title := strings.ToLower(r.Title)
		keep := true
		for _, tok := range tokens {
			if !strings.Contains(title, tok) {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, r)
		}
	}
	return out
}

// DedupAndRank merges duplicate results by info-hash and sorts by seeders.
func DedupAndRank(in []Result) []Result {
	seen := map[string]int{}
	out := make([]Result, 0, len(in))
	for _, r := range in {
		key := strings.ToLower(r.InfoHash)
		if key == "" {
			key = r.Title
		}
		if i, ok := seen[key]; ok {
			if r.Seeders > out[i].Seeders {
				out[i] = mergeResult(r, out[i])
			} else {
				out[i] = mergeResult(out[i], r)
			}
			continue
		}
		seen[key] = len(out)
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seeders > out[j].Seeders })
	return out
}

// mergeResult keeps a as base but pulls non-empty fields from b.
func mergeResult(a, b Result) Result {
	if a.Magnet == "" {
		a.Magnet = b.Magnet
	}
	if a.InfoHash == "" {
		a.InfoHash = b.InfoHash
	}
	if a.Size == 0 {
		a.Size = b.Size
	}
	if a.Date.IsZero() {
		a.Date = b.Date
	}
	a.Source = mergeSources(a.Source, b.Source)
	return a
}

// mergeSources unions two "+"-joined source lists without duplicates, so a
// result merged from several indexers reads e.g. "piratebay+nyaa".
func mergeSources(a, b string) string {
	if a == "" {
		return b
	}
	seen := map[string]bool{}
	for _, s := range strings.Split(a, "+") {
		seen[s] = true
	}
	for _, s := range strings.Split(b, "+") {
		if s != "" && !seen[s] {
			seen[s] = true
			a += "+" + s
		}
	}
	return a
}

// defaultTrackers are public BitTorrent trackers attached to every magnet so
// peer discovery still works when DHT is unavailable. Kept roughly in sync
// with the widely-used community "best trackers" list.
var defaultTrackers = []string{
	"udp://tracker.opentrackr.org:1337/announce",
	"udp://open.demonii.com:1337/announce",
	"udp://open.stealth.si:80/announce",
	"udp://tracker.torrent.eu.org:451/announce",
	"udp://exodus.desync.com:6969/announce",
	"udp://tracker.openbittorrent.com:6969/announce",
	"udp://explodie.org:6969/announce",
	"udp://opentracker.io:6969/announce",
	"udp://tracker.dler.com:6969/announce",
	"udp://tracker-udp.gbitt.info:80/announce",
	"http://tracker.openbittorrent.com:80/announce",
	"https://tracker.tamersunion.org:443/announce",
}

// MagnetFromInfoHash builds a magnet URI with public trackers. extraTrackers,
// when given, are appended to the default set — used to keep a source's own
// tracker (e.g. nyaa's) where the torrent's real peers actually announce.
// Trackers are de-duplicated so an extra tracker already in the default set is
// not added twice.
func MagnetFromInfoHash(infoHash, name string, extraTrackers ...string) string {
	v := url.Values{}
	v.Set("xt", "urn:btih:"+strings.ToLower(infoHash))
	if name != "" {
		v.Set("dn", name)
	}
	seen := map[string]bool{}
	addTracker := func(tr string) {
		if tr == "" || seen[tr] {
			return
		}
		seen[tr] = true
		v.Add("tr", tr)
	}
	for _, tr := range defaultTrackers {
		addTracker(tr)
	}
	for _, tr := range extraTrackers {
		addTracker(tr)
	}
	return "magnet:?" + v.Encode()
}

// HumanRate renders a bytes/sec rate for display.
func HumanRate(bytesPerSec float64) string {
	if bytesPerSec < 1 {
		return "0 B/s"
	}
	return HumanSize(int64(bytesPerSec)) + "/s"
}

// HumanSize renders bytes for display.
func HumanSize(n int64) string {
	if n <= 0 {
		return "?"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
