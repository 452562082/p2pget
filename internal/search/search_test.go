package search

import (
	"net/url"
	"strings"
	"testing"
)

func TestDedupAndRankKeepsHigherSeeders(t *testing.T) {
	in := []Result{
		{Title: "A", InfoHash: "abc", Seeders: 5, Source: "nyaa", Size: 100},
		{Title: "A", InfoHash: "abc", Seeders: 50, Source: "piratebay"},
	}
	out := DedupAndRank(in)
	if len(out) != 1 {
		t.Fatalf("want 1 merged result, got %d", len(out))
	}
	if out[0].Seeders != 50 {
		t.Errorf("merged result kept seeders=%d, want 50 (the higher)", out[0].Seeders)
	}
	if out[0].Size != 100 {
		t.Errorf("merged result lost size, got %d want 100 pulled from the other source", out[0].Size)
	}
}

func TestDedupAndRankSortsBySeeders(t *testing.T) {
	out := DedupAndRank([]Result{
		{Title: "low", InfoHash: "a", Seeders: 1},
		{Title: "high", InfoHash: "b", Seeders: 99},
		{Title: "mid", InfoHash: "c", Seeders: 50},
	})
	if len(out) != 3 || out[0].Seeders != 99 || out[1].Seeders != 50 || out[2].Seeders != 1 {
		t.Errorf("not sorted by seeders desc: %+v", out)
	}
}

func TestFilterByQuery(t *testing.T) {
	in := []Result{
		{Title: "The Matrix (1999) 1080p BluRay"},
		{Title: "The.Boys.S05E08.1080p.WEB"}, // apibay no-match noise
		{Title: "The Matrix Reloaded 2003"},
	}
	out := filterByQuery(in, "the matrix")
	if len(out) != 2 {
		t.Fatalf("query 'the matrix': kept %d, want 2", len(out))
	}
	for _, r := range out {
		if r.Title == "The.Boys.S05E08.1080p.WEB" {
			t.Error("irrelevant 'The Boys' result was not filtered out")
		}
	}

	// CJK: a title with the term is kept, an unrelated one dropped.
	cjk := []Result{
		{Title: "[Dynamis] 海贼王 - 1135 [2160p]"},
		{Title: "One Piece 1135 [1080p]"},
	}
	got := filterByQuery(cjk, "海贼王")
	if len(got) != 1 || got[0].Title != "[Dynamis] 海贼王 - 1135 [2160p]" {
		t.Errorf("query '海贼王': got %v, want only the 海贼王 title", got)
	}

	// Empty query filters nothing.
	if len(filterByQuery(in, "")) != len(in) {
		t.Error("empty query should keep all results")
	}
}

func TestMergeSources(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"piratebay", "nyaa", "piratebay+nyaa"},
		{"piratebay+nyaa", "nyaa", "piratebay+nyaa"},
		{"", "nyaa", "nyaa"},
		{"nyaa", "", "nyaa"},
	}
	for _, c := range cases {
		if got := mergeSources(c.a, c.b); got != c.want {
			t.Errorf("mergeSources(%q,%q)=%q want %q", c.a, c.b, got, c.want)
		}
	}
}

func TestHumanSize(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "?"},
		{-5, "?"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1 << 20, "1.0 MB"},
		{1 << 30, "1.0 GB"},
	}
	for _, c := range cases {
		if got := HumanSize(c.n); got != c.want {
			t.Errorf("HumanSize(%d)=%q want %q", c.n, got, c.want)
		}
	}
}

func TestHumanRate(t *testing.T) {
	if got := HumanRate(0); got != "0 B/s" {
		t.Errorf("HumanRate(0)=%q want 0 B/s", got)
	}
	if got := HumanRate(0.4); got != "0 B/s" {
		t.Errorf("HumanRate(0.4)=%q want 0 B/s", got)
	}
	if got := HumanRate(2 << 20); !strings.HasSuffix(got, "/s") {
		t.Errorf("HumanRate(2MiB)=%q, want a .../s value", got)
	}
}

func TestParseHumanSize(t *testing.T) {
	if got := parseHumanSize("1.5 GiB"); got != int64(1.5*float64(1<<30)) {
		t.Errorf("parseHumanSize(1.5 GiB)=%d", got)
	}
	if got := parseHumanSize("700 MB"); got != 700*(1<<20) {
		t.Errorf("parseHumanSize(700 MB)=%d", got)
	}
	if got := parseHumanSize("garbage"); got != 0 {
		t.Errorf("parseHumanSize(garbage)=%d want 0", got)
	}
}

func TestInfoHashFromLink(t *testing.T) {
	h := infoHashFromLink("magnet:?xt=urn:btih:0123456789ABCDEF0123456789abcdef01234567&dn=x")
	if h != "0123456789abcdef0123456789abcdef01234567" {
		t.Errorf("infoHashFromLink got %q", h)
	}
	if got := infoHashFromLink("no hash here"); got != "" {
		t.Errorf("want empty for no hash, got %q", got)
	}
}

func TestMagnetFromInfoHash(t *testing.T) {
	const hash = "6A9759BFFD5C0AF65319979FB7832189F4F3C35D"
	m := MagnetFromInfoHash(hash, "Some Name")
	if !strings.HasPrefix(m, "magnet:?") {
		t.Errorf("magnet missing scheme: %q", m)
	}
	if !strings.Contains(m, strings.ToLower(hash)) {
		t.Errorf("magnet missing lowercased info-hash: %q", m)
	}
	if !strings.Contains(m, "tr=") {
		t.Errorf("magnet missing trackers: %q", m)
	}
}

func TestMagnetFromInfoHashExtraTrackers(t *testing.T) {
	const hash = "6A9759BFFD5C0AF65319979FB7832189F4F3C35D"
	const extra = "http://nyaa.tracker.wf:7777/announce"
	m := MagnetFromInfoHash(hash, "Name", extra)
	if n := strings.Count(m, url.QueryEscape(extra)); n != 1 {
		t.Errorf("extra tracker %q appears %d times, want 1: %q", extra, n, m)
	}
}

func TestMagnetFromInfoHashDedups(t *testing.T) {
	const hash = "6A9759BFFD5C0AF65319979FB7832189F4F3C35D"
	dup := defaultTrackers[0]
	m := MagnetFromInfoHash(hash, "Name", dup)
	if n := strings.Count(m, url.QueryEscape(dup)); n != 1 {
		t.Errorf("tracker %q appears %d times, want 1: %q", dup, n, m)
	}
}
