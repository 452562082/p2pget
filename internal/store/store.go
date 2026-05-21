// Package store persists DHT-discovered torrent metadata in SQLite.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type Record struct {
	InfoHash string
	Name     string
	Size     int64
	Files    int
	SeenAt   time.Time
}

func Open(path string) (*Store, error) {
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".p2pget", "index.db")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// modernc.org/sqlite serializes writes anyway; pinning to a single
	// connection avoids SQLITE_BUSY churn between concurrent writers and
	// readers (crawler enqueue, replenish, search).
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &Store{db: db}, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS torrents (
  info_hash TEXT PRIMARY KEY,
  name TEXT NOT NULL DEFAULT '',
  size INTEGER NOT NULL DEFAULT 0,
  files INTEGER NOT NULL DEFAULT 0,
  seen_at INTEGER NOT NULL DEFAULT 0,
  resolved INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_torrents_seen ON torrents(seen_at DESC);
CREATE VIRTUAL TABLE IF NOT EXISTS torrents_fts USING fts5(
  info_hash UNINDEXED,
  name,
  tokenize = 'unicode61'
);
`

func (s *Store) Close() error { return s.db.Close() }

// SeenInfoHash records an info-hash discovered via DHT (no metadata yet).
func (s *Store) SeenInfoHash(ctx context.Context, infoHash string) error {
	infoHash = strings.ToLower(infoHash)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO torrents(info_hash, seen_at) VALUES(?, ?)
ON CONFLICT(info_hash) DO UPDATE SET seen_at = excluded.seen_at
`, infoHash, time.Now().Unix())
	return err
}

// SaveMetadata stores the resolved name/size for an info-hash.
func (s *Store) SaveMetadata(ctx context.Context, r Record) error {
	infoHash := strings.ToLower(r.InfoHash)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
INSERT INTO torrents(info_hash, name, size, files, seen_at, resolved) VALUES(?, ?, ?, ?, ?, 1)
ON CONFLICT(info_hash) DO UPDATE SET
  name = excluded.name,
  size = excluded.size,
  files = excluded.files,
  seen_at = MAX(torrents.seen_at, excluded.seen_at),
  resolved = 1
`, infoHash, r.Name, r.Size, r.Files, time.Now().Unix()); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM torrents_fts WHERE info_hash = ?`, infoHash); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO torrents_fts(info_hash, name) VALUES(?, ?)`, infoHash, r.Name); err != nil {
		return err
	}
	return tx.Commit()
}

// Search runs an FTS query.
func (s *Store) Search(ctx context.Context, query string, limit int) ([]Record, error) {
	if limit <= 0 {
		limit = 100
	}
	q := sanitizeFTS(query)
	if q == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT t.info_hash, t.name, t.size, t.files, t.seen_at
FROM torrents_fts f
JOIN torrents t ON t.info_hash = f.info_hash
WHERE torrents_fts MATCH ?
ORDER BY bm25(torrents_fts), t.seen_at DESC
LIMIT ?
`, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var r Record
		var seen int64
		if err := rows.Scan(&r.InfoHash, &r.Name, &r.Size, &r.Files, &seen); err != nil {
			return nil, err
		}
		r.SeenAt = time.Unix(seen, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// PendingMetadata returns up to n recently-seen info-hashes that still need metadata.
func (s *Store) PendingMetadata(ctx context.Context, n int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT info_hash FROM torrents WHERE resolved = 0 ORDER BY seen_at DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Stats returns total/resolved counts.
func (s *Store) Stats(ctx context.Context) (total, resolved int, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(resolved), 0) FROM torrents`).Scan(&total, &resolved)
	return
}

// sanitizeFTS converts free-form input to a safe FTS5 MATCH expression.
func sanitizeFTS(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return ""
	}
	tokens := strings.Fields(q)
	for i, t := range tokens {
		// strip any FTS5 operators by quoting each token
		tokens[i] = `"` + strings.ReplaceAll(t, `"`, "") + `"`
	}
	return strings.Join(tokens, " ")
}
