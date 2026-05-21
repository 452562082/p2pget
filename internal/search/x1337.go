package search

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// X1337 scrapes 1337x.to. It may be blocked in some regions; failures are non-fatal
// when used via Aggregator.
type X1337 struct {
	Endpoint string
	Client   *http.Client
	// MaxDetail caps the number of detail-page fetches per search. 0 disables them.
	MaxDetail int
	// Solver, when set, routes page fetches through a Cloudflare solver
	// (FlareSolverr) instead of a direct HTTP GET.
	Solver Solver
}

func NewX1337() *X1337 {
	return &X1337{
		Endpoint:  "https://1337x.to",
		Client:    &http.Client{Timeout: 15 * time.Second},
		MaxDetail: 10,
	}
}

func (x *X1337) Name() string { return "1337x" }

func (x *X1337) Search(ctx context.Context, query string) ([]Result, error) {
	u := fmt.Sprintf("%s/search/%s/1/", x.Endpoint, url.PathEscape(query))
	doc, err := x.fetchDoc(ctx, u)
	if err != nil {
		return nil, err
	}

	var results []Result
	var details []string

	doc.Find("table.table-list tbody tr").Each(func(_ int, s *goquery.Selection) {
		titleA := s.Find("td.coll-1.name a").Eq(1)
		title := strings.TrimSpace(titleA.Text())
		href, _ := titleA.Attr("href")
		if title == "" || href == "" {
			return
		}
		seeders, _ := strconv.Atoi(strings.TrimSpace(s.Find("td.coll-2.seeds").Text()))
		leechers, _ := strconv.Atoi(strings.TrimSpace(s.Find("td.coll-3.leeches").Text()))
		size := parseHumanSize(strings.TrimSpace(s.Find("td.coll-4.size").Contents().First().Text()))

		results = append(results, Result{
			Title:    title,
			Size:     size,
			Seeders:  seeders,
			Leechers: leechers,
			Source:   x.Name(),
		})
		details = append(details, x.Endpoint+href)
	})

	// Resolve magnets in parallel, bounded.
	limit := min(x.MaxDetail, len(results))
	if limit > 0 {
		x.fillMagnets(ctx, results[:limit], details[:limit])
	}

	// Drop entries we couldn't get a magnet for.
	out := results[:0]
	for _, r := range results {
		if r.InfoHash != "" {
			out = append(out, r)
		}
	}
	return out, nil
}

func (x *X1337) fillMagnets(ctx context.Context, rs []Result, urls []string) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i := range rs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			magnet, hash, err := x.fetchMagnet(ctx, urls[i])
			if err != nil {
				return
			}
			rs[i].Magnet = magnet
			rs[i].InfoHash = hash
		}(i)
	}
	wg.Wait()
}

func (x *X1337) fetchMagnet(ctx context.Context, pageURL string) (string, string, error) {
	doc, err := x.fetchDoc(ctx, pageURL)
	if err != nil {
		return "", "", err
	}
	var magnet string
	doc.Find("a[href^='magnet:']").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		if href, ok := s.Attr("href"); ok {
			magnet = href
			return false
		}
		return true
	})
	if magnet == "" {
		return "", "", fmt.Errorf("no magnet on %s", pageURL)
	}
	hash := infoHashFromLink(magnet)
	return magnet, hash, nil
}

func (x *X1337) fetchDoc(ctx context.Context, url string) (*goquery.Document, error) {
	if x.Solver != nil {
		html, err := x.Solver.Get(ctx, url)
		if err != nil {
			return nil, err
		}
		return goquery.NewDocumentFromReader(strings.NewReader(html))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := x.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d (likely Cloudflare-blocked)", resp.StatusCode)
	}
	return goquery.NewDocumentFromReader(resp.Body)
}
