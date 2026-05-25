package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"

	"p2pget/internal/engine"
	"p2pget/internal/search"
)

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("short string changed: %q", got)
	}
	if got := truncate("hello world", 0); got != "hello world" {
		t.Errorf("w<=0 should return input unchanged: %q", got)
	}
	if w := runewidth.StringWidth(truncate("hello world", 8)); w > 8 {
		t.Errorf("truncate(ascii) width=%d exceeds 8", w)
	}
	// CJK characters are two columns wide; truncation must respect that.
	cjk := truncate("电影动漫合集测试", 7)
	if w := runewidth.StringWidth(cjk); w > 7 {
		t.Errorf("truncate(CJK) width=%d exceeds 7: %q", w, cjk)
	}
}

func TestFormatETA(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{90 * time.Second, "1m30s"},
		{3*time.Hour + 5*time.Minute, "3h05m"},
	}
	for _, c := range cases {
		if got := formatETA(c.d); got != c.want {
			t.Errorf("formatETA(%v)=%q want %q", c.d, got, c.want)
		}
	}
}

func TestSameDownloads(t *testing.T) {
	a, b := &engine.Download{}, &engine.Download{}
	if !sameDownloads(nil, nil) {
		t.Error("nil vs nil should be equal")
	}
	if !sameDownloads([]*engine.Download{a, b}, []*engine.Download{a, b}) {
		t.Error("identical pointer slices should be equal")
	}
	if sameDownloads([]*engine.Download{a}, []*engine.Download{b}) {
		t.Error("different pointers should not be equal")
	}
	if sameDownloads([]*engine.Download{a}, []*engine.Download{a, b}) {
		t.Error("different lengths should not be equal")
	}
}

func TestModelTabSwitch(t *testing.T) {
	eng, err := engine.New(engine.Config{DataDir: t.TempDir(), NoDHT: true})
	if err != nil {
		t.Skipf("engine unavailable in this environment: %v", err)
	}
	defer eng.Close()

	m := New(eng, search.New())
	if m.active != tabSearch {
		t.Fatalf("initial tab=%d, want tabSearch", m.active)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m.active != tabDownloads {
		t.Errorf("after Tab: active=%d, want tabDownloads", m.active)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m.active != tabSearch {
		t.Errorf("after three Tabs: active=%d, want wrap back to tabSearch", m.active)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	if m.active != tabHelp {
		t.Errorf("after Shift+Tab from first tab: active=%d, want wrap to tabHelp", m.active)
	}
}

func TestIsCompleted(t *testing.T) {
	cases := []struct {
		name string
		s    engine.Status
		want bool
	}{
		{"no metadata", engine.Status{}, false},
		{"metadata but zero total", engine.Status{HaveInfo: true}, false},
		{"in progress", engine.Status{HaveInfo: true, TotalBytes: 100, BytesDone: 50}, false},
		{"exact complete", engine.Status{HaveInfo: true, TotalBytes: 100, BytesDone: 100}, true},
		{"over-complete still complete", engine.Status{HaveInfo: true, TotalBytes: 100, BytesDone: 120}, true},
	}
	for _, c := range cases {
		if got := isCompleted(c.s); got != c.want {
			t.Errorf("%s: isCompleted=%v want %v", c.name, got, c.want)
		}
	}
}

// TestPruneCompletedKeepsActive guards against an over-eager prune that would
// drop tasks before metadata arrives or while bytes are still flowing — the
// auto-remove must only fire when the download is genuinely done.
func TestPruneCompletedKeepsActive(t *testing.T) {
	eng, err := engine.New(engine.Config{DataDir: t.TempDir(), NoDHT: true})
	if err != nil {
		t.Skipf("engine unavailable in this environment: %v", err)
	}
	defer eng.Close()

	if _, err := eng.AddInfoHash("0123456789abcdef0123456789abcdef01234567"); err != nil {
		t.Fatalf("AddInfoHash: %v", err)
	}

	m := New(eng, search.New())
	m.pruneCompleted()
	if got := len(eng.List()); got != 1 {
		t.Fatalf("pruneCompleted dropped an active download: got %d, want 1", got)
	}
}

// TestRestartStalledIgnoresPreMetadata guards the watchdog against firing
// before metadata arrives — that case is owned by awaitMetadata's timeout and
// auto-restarting there would spin forever for any genuinely dead info-hash.
func TestRestartStalledIgnoresPreMetadata(t *testing.T) {
	eng, err := engine.New(engine.Config{
		DataDir:         t.TempDir(),
		NoDHT:           true,
		NoUpload:        true,
		MetadataTimeout: -1,
	})
	if err != nil {
		t.Skipf("engine unavailable in this environment: %v", err)
	}
	defer eng.Close()

	const hash = "0123456789abcdef0123456789abcdef01234567"
	d1, err := eng.AddInfoHash(hash)
	if err != nil {
		t.Fatalf("AddInfoHash: %v", err)
	}

	m := New(eng, search.New())
	m.restartStalled()

	list := eng.List()
	if len(list) != 1 || list[0] != d1 {
		t.Fatalf("restartStalled touched a pre-metadata download (was %p, list=%v)", d1, list)
	}
}

// TestPressDOnDownloadsTabRemovesTask covers the bug where the 'd' shortcut
// silently did nothing if the user Tab-switched to the downloads tab without
// first blurring the search input. The shortcut's guard required
// !m.input.Focused(), but the input retains focus across tab switches even
// though it isn't displayed on the downloads tab — so 'd' became a no-op for
// any user who hadn't run a search this session.
func TestPressDOnDownloadsTabRemovesTask(t *testing.T) {
	eng, err := engine.New(engine.Config{DataDir: t.TempDir(), NoDHT: true})
	if err != nil {
		t.Skipf("engine unavailable in this environment: %v", err)
	}
	defer eng.Close()

	if _, err := eng.AddInfoHash("0123456789abcdef0123456789abcdef01234567"); err != nil {
		t.Fatalf("AddInfoHash: %v", err)
	}
	if got := len(eng.List()); got != 1 {
		t.Fatalf("setup: expected 1 download, got %d", got)
	}

	m := New(eng, search.New())
	m.refreshDownloads()

	// Tab from search → downloads. Critically, the search input remains
	// focused (default state on a freshly opened app) — this is the path
	// that previously made 'd' a no-op.
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m.active != tabDownloads {
		t.Fatalf("after Tab: active=%d, want tabDownloads", m.active)
	}

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})

	if got := len(eng.List()); got != 0 {
		t.Fatalf("after 'd' on downloads tab: expected 0 downloads, got %d (remove did not fire)", got)
	}
}
