package search

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Jackett queries a Jackett instance via its Torznab API. A single Jackett
// fans out to every tracker the user has configured there (public and
// private), so one adapter covers many sources at once.
type Jackett struct {
	Endpoint string // Jackett base URL, e.g. http://localhost:9117
	APIKey   string
	Client   *http.Client
}

func NewJackett(endpoint, apiKey string) *Jackett {
	if endpoint == "" {
		endpoint = "http://localhost:9117"
	}
	return &Jackett{
		Endpoint: strings.TrimRight(endpoint, "/"),
		APIKey:   apiKey,
		Client:   &http.Client{Timeout: 25 * time.Second},
	}
}

func (j *Jackett) Name() string { return "jackett" }

type torznabFeed struct {
	XMLName xml.Name      `xml:"rss"`
	Items   []torznabItem `xml:"channel>item"`
}

type torznabItem struct {
	Title     string        `xml:"title"`
	Link      string        `xml:"link"`
	PubDate   string        `xml:"pubDate"`
	Size      int64         `xml:"size"`
	Enclosure torznabEnc    `xml:"enclosure"`
	Attrs     []torznabAttr `xml:"attr"`
}

type torznabEnc struct {
	URL string `xml:"url,attr"`
}

type torznabAttr struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

func (j *Jackett) Search(ctx context.Context, query string) ([]Result, error) {
	u := fmt.Sprintf("%s/api/v2.0/indexers/all/results/torznab/api?apikey=%s&t=search&q=%s",
		j.Endpoint, url.QueryEscape(j.APIKey), url.QueryEscape(query))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := j.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("jackett status %d: %s", resp.StatusCode, body)
	}

	dec := xml.NewDecoder(resp.Body)
	dec.Strict = false
	var feed torznabFeed
	if err := dec.Decode(&feed); err != nil {
		return nil, err
	}

	out := make([]Result, 0, len(feed.Items))
	for _, it := range feed.Items {
		r := Result{Title: it.Title, Size: it.Size, Source: j.Name()}
		for _, a := range it.Attrs {
			switch strings.ToLower(a.Name) {
			case "seeders":
				r.Seeders, _ = strconv.Atoi(a.Value)
			case "peers", "leechers":
				r.Leechers, _ = strconv.Atoi(a.Value)
			case "infohash":
				r.InfoHash = strings.ToLower(strings.TrimSpace(a.Value))
			case "magneturl":
				if strings.HasPrefix(a.Value, "magnet:") {
					r.Magnet = a.Value
				}
			case "size":
				if r.Size == 0 {
					r.Size, _ = strconv.ParseInt(a.Value, 10, 64)
				}
			}
		}
		if r.Magnet == "" {
			for _, cand := range []string{it.Enclosure.URL, it.Link} {
				if strings.HasPrefix(cand, "magnet:") {
					r.Magnet = cand
					break
				}
			}
		}
		if r.InfoHash == "" && r.Magnet != "" {
			r.InfoHash = infoHashFromLink(r.Magnet)
		}
		if r.Magnet == "" && len(r.InfoHash) == 40 {
			r.Magnet = MagnetFromInfoHash(r.InfoHash, r.Title)
		}
		// Skip results that carry neither a magnet nor an info-hash: this
		// engine downloads via DHT/magnet and cannot use a tracker-only
		// .torrent download link.
		if r.Magnet == "" && r.InfoHash == "" {
			continue
		}
		if t, err := time.Parse(time.RFC1123Z, it.PubDate); err == nil {
			r.Date = t
		}
		out = append(out, r)
	}
	return out, nil
}
