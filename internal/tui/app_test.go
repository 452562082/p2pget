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
