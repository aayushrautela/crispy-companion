package engine_test

import (
	"strings"
	"testing"

	"github.com/crispy-streaming/crispy-companion/internal/engine"
)

func testStatus() engine.TorrentStatus {
	return engine.TorrentStatus{
		Name: "multi",
		Hash: "ab12",
		Stat: 3,
		FileStats: []engine.FileStat{
			{ID: 1, Path: "a/b.mp4", Length: 1000},
			{ID: 2, Path: "a/c.mp4", Length: 3000},
		},
	}
}

func TestFileByIndexBounds(t *testing.T) {
	st := testStatus()
	f, err := engine.FileByIndex(st, 0)
	if err != nil || f.ID != 1 {
		t.Fatalf("idx 0 → %+v, %v", f, err)
	}
	f, err = engine.FileByIndex(st, 1)
	if err != nil || f.ID != 2 {
		t.Fatalf("idx 1 → %+v, %v", f, err)
	}
	if _, err := engine.FileByIndex(st, 2); err == nil {
		t.Fatal("idx 2 should fail")
	}
	if _, err := engine.FileByIndex(st, -1); err == nil {
		t.Fatal("idx -1 should fail")
	}
}

func TestProgressForFile(t *testing.T) {
	st := testStatus()
	// Piece length 1000: file 1 = piece 0; file 2 = pieces 1,2,3.
	cs := engine.CacheState{
		PiecesLength: 1000,
		PiecesCount:  4,
		Pieces: map[string]engine.PieceState{
			"0": {Completed: true, Length: 1000},
			"1": {Completed: true, Length: 1000},
			"2": {Completed: false, Length: 1000},
			// "3" missing entirely → incomplete
		},
	}
	f1, _ := engine.FileByIndex(st, 0)
	if p := engine.ProgressForFile(st, cs, f1); p != 1 {
		t.Fatalf("file 1 progress = %v, want 1", p)
	}
	f2, _ := engine.FileByIndex(st, 1)
	// file 2 spans pieces 1..3, only piece 1 complete → 1000/3000.
	if p := engine.ProgressForFile(st, cs, f2); p < 0.333 || p > 0.334 {
		t.Fatalf("file 2 progress = %v, want ~1/3", p)
	}
	if d := engine.DownloadedForFile(st, cs, f2); d != 1000 {
		t.Fatalf("file 2 downloaded = %d, want 1000", d)
	}
	// Empty cache → zero, never NaN.
	empty := engine.CacheState{}
	if p := engine.ProgressForFile(st, empty, f1); p != 0 {
		t.Fatalf("empty cache progress = %v, want 0", p)
	}
}

func TestMagnetForHash(t *testing.T) {
	m := engine.MagnetForHash("AB12CD34EF56AB12CD34EF56AB12CD34EF56AB12")
	if !strings.HasPrefix(m, "magnet:?xt=urn:btih:ab12cd34") {
		t.Fatalf("bad magnet: %s", m)
	}
	if !strings.Contains(m, "tr=") {
		t.Fatalf("magnet lacks trackers: %s", m)
	}
}
