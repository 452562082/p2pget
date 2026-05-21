package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// PirateBay searches via apibay.org, a public Pirate Bay-compatible JSON API.
type PirateBay struct {
	Endpoint string // default https://apibay.org
	Client   *http.Client
}

func NewPirateBay() *PirateBay {
	return &PirateBay{
		Endpoint: "https://apibay.org",
		Client:   &http.Client{Timeout: 12 * time.Second},
	}
}

func (p *PirateBay) Name() string { return "piratebay" }

type pirateBayItem struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	InfoHash string `json:"info_hash"`
	Leechers string `json:"leechers"`
	Seeders  string `json:"seeders"`
	NumFiles string `json:"num_files"`
	Size     string `json:"size"`
	Username string `json:"username"`
	Added    string `json:"added"`
	Status   string `json:"status"`
	Category string `json:"category"`
}

func (p *PirateBay) Search(ctx context.Context, query string) ([]Result, error) {
	u := p.Endpoint + "/q.php?q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("apibay status %d: %s", resp.StatusCode, body)
	}
	var raw []pirateBayItem
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}

	out := make([]Result, 0, len(raw))
	for _, it := range raw {
		// apibay returns a sentinel entry with id="0" when no results.
		if it.ID == "0" || it.InfoHash == "" || it.InfoHash == "0000000000000000000000000000000000000000" {
			continue
		}
		size, _ := strconv.ParseInt(it.Size, 10, 64)
		seeders, _ := strconv.Atoi(it.Seeders)
		leechers, _ := strconv.Atoi(it.Leechers)
		added, _ := strconv.ParseInt(it.Added, 10, 64)

		out = append(out, Result{
			Title:    it.Name,
			InfoHash: it.InfoHash,
			Magnet:   MagnetFromInfoHash(it.InfoHash, it.Name),
			Size:     size,
			Seeders:  seeders,
			Leechers: leechers,
			Source:   p.Name(),
			Date:     time.Unix(added, 0),
		})
	}
	return out, nil
}

const userAgent = "p2pget/0.1 (+https://github.com/) Go-http-client"
