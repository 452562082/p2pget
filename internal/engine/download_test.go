package engine

import (
	"errors"
	"testing"
	"time"
)

func TestStatusETA(t *testing.T) {
	if d := (Status{}).ETA(); d != 0 {
		t.Errorf("no metadata: ETA=%v want 0", d)
	}
	complete := Status{HaveInfo: true, TotalBytes: 100, BytesDone: 100, DownRate: 10}
	if d := complete.ETA(); d != 0 {
		t.Errorf("complete: ETA=%v want 0", d)
	}
	normal := Status{HaveInfo: true, TotalBytes: 100, BytesDone: 0, DownRate: 10}
	if d := normal.ETA(); d != 10*time.Second {
		t.Errorf("100 bytes at 10 B/s: ETA=%v want 10s", d)
	}
	zeroRate := Status{HaveInfo: true, TotalBytes: 100, DownRate: 0}
	if d := zeroRate.ETA(); d != 0 {
		t.Errorf("zero rate: ETA=%v want 0", d)
	}
	huge := Status{HaveInfo: true, TotalBytes: 1 << 50, DownRate: 1}
	if d := huge.ETA(); d != 0 {
		t.Errorf("huge estimate should clamp to 0 (overflow guard), got %v", d)
	}
}

func TestStatusProgress(t *testing.T) {
	if p := (Status{}).Progress(); p != 0 {
		t.Errorf("no metadata: Progress=%v want 0", p)
	}
	s := Status{HaveInfo: true, TotalBytes: 200, BytesDone: 50}
	if p := s.Progress(); p != 0.25 {
		t.Errorf("Progress=%v want 0.25", p)
	}
}

func TestErrString(t *testing.T) {
	if s := errString(nil); s != "" {
		t.Errorf("errString(nil)=%q want empty", s)
	}
	if s := errString(errors.New("boom")); s != "boom" {
		t.Errorf("errString=%q want boom", s)
	}
}

func TestRateBurst(t *testing.T) {
	if b := rateBurst(100); b != 256<<10 {
		t.Errorf("rateBurst(100)=%d want minimum 256KiB", b)
	}
	if b := rateBurst(1 << 20); b != 1<<20 {
		t.Errorf("rateBurst(1MiB)=%d want the rate itself", b)
	}
}
