package search

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Nyaa searches nyaa.si via its RSS feed.
type Nyaa struct {
	Endpoint string
	Client   *http.Client
}

func NewNyaa() *Nyaa {
	return &Nyaa{
		Endpoint: "https://nyaa.si",
		Client:   &http.Client{Timeout: 12 * time.Second},
	}
}

func (n *Nyaa) Name() string { return "nyaa" }

type nyaaRSS struct {
	XMLName xml.Name   `xml:"rss"`
	Channel nyaaChan   `xml:"channel"`
}

type nyaaChan struct {
	Items []nyaaItem `xml:"item"`
}

type nyaaItem struct {
	Title    string `xml:"title"`
	Link     string `xml:"link"`
	GUID     string `xml:"guid"`
	PubDate  string `xml:"pubDate"`
	Seeders  string `xml:"seeders"`
	Leechers string `xml:"leechers"`
	Size     string `xml:"size"`
	InfoHash string `xml:"infoHash"`
}

func (n *Nyaa) Search(ctx context.Context, query string) ([]Result, error) {
	u := fmt.Sprintf("%s/?page=rss&q=%s&c=0_0&f=0", n.Endpoint, url.QueryEscape(query))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := n.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("nyaa status %d: %s", resp.StatusCode, body)
	}

	// Nyaa uses custom namespace "nyaa:" — Go's encoding/xml ignores namespace prefixes
	// when struct tags use the local name, but PubDate/etc are plain. Decode with a
	// loose strategy and re-extract namespaced fields from raw XML if needed.
	dec := xml.NewDecoder(resp.Body)
	dec.Strict = false
	var feed nyaaRSS
	if err := dec.Decode(&feed); err != nil {
		return nil, err
	}

	out := make([]Result, 0, len(feed.Channel.Items))
	for _, it := range feed.Channel.Items {
		hash := strings.ToLower(strings.TrimSpace(it.InfoHash))
		if hash == "" {
			hash = infoHashFromLink(it.Link)
		}
		if hash == "" {
			continue
		}
		seeders, _ := strconv.Atoi(it.Seeders)
		leechers, _ := strconv.Atoi(it.Leechers)
		pubDate, _ := time.Parse(time.RFC1123Z, it.PubDate)
		if pubDate.IsZero() {
			pubDate, _ = time.Parse(time.RFC1123, it.PubDate)
		}
		out = append(out, Result{
			Title:    it.Title,
			InfoHash: hash,
			Magnet:   MagnetFromInfoHash(hash, it.Title, "http://nyaa.tracker.wf:7777/announce"),
			Size:     parseHumanSize(it.Size),
			Seeders:  seeders,
			Leechers: leechers,
			Source:   n.Name(),
			Date:     pubDate,
		})
	}
	return out, nil
}

var sizeRE = regexp.MustCompile(`(?i)^\s*([\d.]+)\s*([KMGT]?i?B)\s*$`)

func parseHumanSize(s string) int64 {
	m := sizeRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	unit := strings.ToUpper(m[2])
	mult := float64(1)
	switch unit {
	case "B":
		mult = 1
	case "KB", "KIB":
		mult = 1 << 10
	case "MB", "MIB":
		mult = 1 << 20
	case "GB", "GIB":
		mult = 1 << 30
	case "TB", "TIB":
		mult = 1 << 40
	}
	return int64(v * mult)
}

var infoHashRE = regexp.MustCompile(`(?i)([a-f0-9]{40})`)

func infoHashFromLink(s string) string {
	m := infoHashRE.FindString(s)
	return strings.ToLower(m)
}
