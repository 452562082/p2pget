package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TorrentsCSV queries torrents-csv.com, an open community torrent index with a
// clean public JSON API — no auth, no Cloudflare. General-purpose content.
type TorrentsCSV struct {
	Endpoint string
	Client   *http.Client
}

func NewTorrentsCSV() *TorrentsCSV {
	return &TorrentsCSV{
		Endpoint: "https://torrents-csv.com",
		Client:   &http.Client{Timeout: 12 * time.Second},
	}
}

func (t *TorrentsCSV) Name() string { return "torrents-csv" }

type torrentsCSVResp struct {
	Torrents []torrentsCSVItem `json:"torrents"`
}

type torrentsCSVItem struct {
	InfoHash    string `json:"infohash"`
	Name        string `json:"name"`
	SizeBytes   int64  `json:"size_bytes"`
	Seeders     int    `json:"seeders"`
	Leechers    int    `json:"leechers"`
	CreatedUnix int64  `json:"created_unix"`
}

func (t *TorrentsCSV) Search(ctx context.Context, query string) ([]Result, error) {
	u := fmt.Sprintf("%s/service/search?size=100&q=%s", t.Endpoint, url.QueryEscape(query))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := t.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("torrents-csv status %d: %s", resp.StatusCode, body)
	}
	var raw torrentsCSVResp
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}

	out := make([]Result, 0, len(raw.Torrents))
	for _, it := range raw.Torrents {
		hash := strings.ToLower(strings.TrimSpace(it.InfoHash))
		if len(hash) != 40 {
			continue
		}
		out = append(out, Result{
			Title:    it.Name,
			InfoHash: hash,
			Magnet:   MagnetFromInfoHash(hash, it.Name),
			Size:     it.SizeBytes,
			Seeders:  it.Seeders,
			Leechers: it.Leechers,
			Source:   t.Name(),
			Date:     time.Unix(it.CreatedUnix, 0),
		})
	}
	return out, nil
}
