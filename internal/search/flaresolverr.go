package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Solver fetches a URL's fully-rendered HTML, used to get past anti-bot
// challenges (e.g. Cloudflare) that defeat a plain HTTP GET.
type Solver interface {
	Get(ctx context.Context, target string) (string, error)
}

// FlareSolverr drives a FlareSolverr service, which runs a headless browser to
// solve Cloudflare challenges and returns the resulting page HTML.
type FlareSolverr struct {
	Endpoint string // base URL, e.g. http://localhost:8191
	Client   *http.Client
}

func NewFlareSolverr(endpoint string) *FlareSolverr {
	return &FlareSolverr{
		Endpoint: strings.TrimRight(endpoint, "/"),
		Client:   &http.Client{Timeout: 90 * time.Second},
	}
}

type flareRequest struct {
	Cmd        string `json:"cmd"`
	URL        string `json:"url"`
	MaxTimeout int    `json:"maxTimeout"`
}

type flareResponse struct {
	Status   string `json:"status"`
	Message  string `json:"message"`
	Solution struct {
		Status   int    `json:"status"`
		Response string `json:"response"`
	} `json:"solution"`
}

// Get returns the HTML of target as seen after FlareSolverr clears any
// anti-bot interstitial.
func (f *FlareSolverr) Get(ctx context.Context, target string) (string, error) {
	payload, err := json.Marshal(flareRequest{
		Cmd:        "request.get",
		URL:        target,
		MaxTimeout: 60000,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.Endpoint+"/v1", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("flaresolverr HTTP %d", resp.StatusCode)
	}
	var fr flareResponse
	if err := json.Unmarshal(body, &fr); err != nil {
		return "", err
	}
	if fr.Status != "ok" {
		return "", fmt.Errorf("flaresolverr: %s", fr.Message)
	}
	if fr.Solution.Status >= 400 {
		return "", fmt.Errorf("flaresolverr: target returned %d", fr.Solution.Status)
	}
	return fr.Solution.Response, nil
}
