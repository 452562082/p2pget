package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestStoreRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	const hash = "0123456789abcdef0123456789abcdef01234567"

	if err := s.SeenInfoHash(ctx, hash); err != nil {
		t.Fatalf("SeenInfoHash: %v", err)
	}
	total, resolved, err := s.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if total != 1 || resolved != 0 {
		t.Errorf("after SeenInfoHash: total=%d resolved=%d, want 1/0", total, resolved)
	}

	pend, err := s.PendingMetadata(ctx, 10)
	if err != nil {
		t.Fatalf("PendingMetadata: %v", err)
	}
	if len(pend) != 1 || pend[0] != hash {
		t.Errorf("PendingMetadata=%v, want [%s]", pend, hash)
	}

	if err := s.SaveMetadata(ctx, Record{
		InfoHash: hash, Name: "Ubuntu 24.04 Desktop", Size: 5000, Files: 1,
	}); err != nil {
		t.Fatalf("SaveMetadata: %v", err)
	}
	total, resolved, _ = s.Stats(ctx)
	if total != 1 || resolved != 1 {
		t.Errorf("after SaveMetadata: total=%d resolved=%d, want 1/1", total, resolved)
	}
	if pend, _ := s.PendingMetadata(ctx, 10); len(pend) != 0 {
		t.Errorf("PendingMetadata after resolve=%v, want empty", pend)
	}

	recs, err := s.Search(ctx, "ubuntu", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(recs) != 1 || recs[0].Name != "Ubuntu 24.04 Desktop" {
		t.Errorf("Search(ubuntu)=%v, want one Ubuntu record", recs)
	}

	if recs, _ := s.Search(ctx, "nonexistentword", 10); len(recs) != 0 {
		t.Errorf("Search(nonexistentword)=%v, want no match", recs)
	}
}

func TestSaveMetadataUpdatesFTS(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	const hash = "1111111111111111111111111111111111111111"

	if err := s.SaveMetadata(ctx, Record{InfoHash: hash, Name: "old name", Size: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveMetadata(ctx, Record{InfoHash: hash, Name: "new title", Size: 2}); err != nil {
		t.Fatal(err)
	}
	// The rename must not leave a stale FTS row behind.
	if recs, _ := s.Search(ctx, "old", 10); len(recs) != 0 {
		t.Errorf("old name still searchable after rename: %v", recs)
	}
	recs, _ := s.Search(ctx, "title", 10)
	if len(recs) != 1 {
		t.Errorf("Search(title)=%v, want one match", recs)
	}
}

func TestSanitizeFTS(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"ubuntu", `"ubuntu"`},
		{`ubuntu "24.04"`, `"ubuntu" "24.04"`},
	}
	for _, c := range cases {
		if got := sanitizeFTS(c.in); got != c.want {
			t.Errorf("sanitizeFTS(%q)=%q want %q", c.in, got, c.want)
		}
	}
}
