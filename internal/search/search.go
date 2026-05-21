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
			out = append(out, res...)
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
			ch <- IndexerResult{Name: idx.Name(), Results: res, Err: err}
		}(idx)
	}
	go func() {
		wg.Wait()
		cancel()
		close(ch)
	}()
	return ch
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

// MagnetFromInfoHash builds a magnet URI with sane public trackers.
func MagnetFromInfoHash(infoHash, name string) string {
	trackers := []string{
		"udp://tracker.opentrackr.org:1337/announce",
		"udp://tracker.openbittorrent.com:6969/announce",
		"udp://exodus.desync.com:6969/announce",
		"udp://open.stealth.si:80/announce",
		"udp://tracker.torrent.eu.org:451/announce",
	}
	v := url.Values{}
	v.Set("xt", "urn:btih:"+strings.ToLower(infoHash))
	if name != "" {
		v.Set("dn", name)
	}
	for _, t := range trackers {
		v.Add("tr", t)
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
