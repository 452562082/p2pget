// Package dht runs a passive DHT crawler that harvests info-hashes from
// incoming GET_PEERS / ANNOUNCE_PEER queries, then resolves their metadata
// via BEP9 using the torrent engine.
package dht

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/dht/v2/krpc"
	"github.com/anacrolix/torrent/metainfo"

	"p2pget/internal/engine"
	"p2pget/internal/search"
	"p2pget/internal/store"
)

type Crawler struct {
	Store  *store.Store
	Engine *engine.Engine
	Logger *log.Logger

	// MetadataWorkers is the number of concurrent BEP9 fetches.
	MetadataWorkers int
	// MetadataTimeout per info-hash.
	MetadataTimeout time.Duration

	server *dht.Server

	recent *recentSet
	seen   chan string
}

func New(s *store.Store, e *engine.Engine) *Crawler {
	return &Crawler{
		Store:           s,
		Engine:          e,
		MetadataWorkers: 8,
		MetadataTimeout: 60 * time.Second,
		recent:          newRecentSet(50_000),
		seen:            make(chan string, 1024),
	}
}

// Start launches the DHT server and metadata workers until ctx is canceled.
func (c *Crawler) Start(ctx context.Context) error {
	cfg := dht.NewDefaultServerConfig()
	cfg.OnQuery = c.onQuery
	cfg.OnAnnouncePeer = c.onAnnouncePeer

	srv, err := dht.NewServer(cfg)
	if err != nil {
		return fmt.Errorf("dht server: %w", err)
	}
	c.server = srv
	c.logf("dht crawler listening on %v", srv.Addr())

	// Kick off bootstrap.
	go func() {
		ts, err := srv.Bootstrap()
		c.logf("dht bootstrap: %+v err=%v", ts, err)
	}()

	// Metadata workers
	var wg sync.WaitGroup
	for i := 0; i < c.MetadataWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.metadataWorker(ctx)
		}()
	}

	// Periodically scan for pending info-hashes the queue might have missed.
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.replenishLoop(ctx)
	}()

	<-ctx.Done()
	srv.Close()
	wg.Wait()
	return nil
}

func (c *Crawler) onQuery(msg *krpc.Msg, _ net.Addr) bool {
	if msg == nil || msg.A == nil {
		return true
	}
	id := msg.A.InfoHash
	var zero krpc.ID
	if id == zero {
		return true
	}
	c.enqueue(metainfo.Hash(id))
	return true
}

func (c *Crawler) onAnnouncePeer(infoHash metainfo.Hash, _ net.IP, _ int, _ bool) {
	c.enqueue(infoHash)
}

func (c *Crawler) enqueue(h metainfo.Hash) {
	hex := h.HexString()
	if hex == "0000000000000000000000000000000000000000" {
		return
	}
	// Drop traffic-driven duplicates without touching SQLite. The DHT
	// emits the same info-hashes repeatedly; storing each one is the
	// dominant cost of the crawler.
	if c.recent.seen(hex) {
		return
	}
	if err := c.Store.SeenInfoHash(context.Background(), hex); err != nil {
		c.logf("store seen: %v", err)
		return
	}
	select {
	case c.seen <- hex:
	default:
		// queue full; replenishLoop will pick it up later
	}
}

func (c *Crawler) metadataWorker(ctx context.Context) {
	for {
		var hex string
		select {
		case <-ctx.Done():
			return
		case h, ok := <-c.seen:
			if !ok {
				return
			}
			hex = h
		}
		fetchCtx, cancel := context.WithTimeout(ctx, c.MetadataTimeout)
		info, err := c.Engine.FetchInfo(fetchCtx, hex)
		cancel()
		if err != nil || info == nil {
			continue
		}
		rec := store.Record{
			InfoHash: hex,
			Name:     info.BestName(),
			Size:     info.TotalLength(),
			Files:    len(info.Files),
		}
		if rec.Files == 0 {
			rec.Files = 1
		}
		if err := c.Store.SaveMetadata(context.Background(), rec); err != nil {
			c.logf("store metadata %s: %v", hex, err)
			continue
		}
		c.logf("resolved %s — %s (%d files)", hex[:10], rec.Name, rec.Files)
	}
}

func (c *Crawler) replenishLoop(ctx context.Context) {
	t := time.NewTicker(2 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pending, err := c.Store.PendingMetadata(ctx, 64)
			if err != nil {
				c.logf("pending: %v", err)
				continue
			}
			for _, h := range pending {
				if ctx.Err() != nil {
					return
				}
				select {
				case c.seen <- h:
				default:
				}
			}
		}
	}
}

func (c *Crawler) logf(f string, args ...any) {
	if c.Logger != nil {
		c.Logger.Printf(f, args...)
	}
}

// StoreIndexer adapts the SQLite store as a search.Indexer.
type StoreIndexer struct {
	Store *store.Store
	Limit int
}

func (s *StoreIndexer) Name() string { return "dht-index" }

func (s *StoreIndexer) Search(ctx context.Context, query string) ([]search.Result, error) {
	limit := s.Limit
	if limit <= 0 {
		limit = 200
	}
	recs, err := s.Store.Search(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	out := make([]search.Result, 0, len(recs))
	for _, r := range recs {
		out = append(out, search.Result{
			Title:    r.Name,
			InfoHash: r.InfoHash,
			Magnet:   search.MagnetFromInfoHash(r.InfoHash, r.Name),
			Size:     r.Size,
			Source:   "dht-index",
			Date:     r.SeenAt,
		})
	}
	return out, nil
}
