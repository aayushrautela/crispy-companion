package engine_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// TestContiguousForFile pins the playback-runway count: bytes are counted
// from the file's first byte and stop at the first incomplete piece, so a
// half-filled file reports no runway rather than partial credit.
func TestContiguousForFile(t *testing.T) {
	st := testStatus()
	cs := engine.CacheState{
		PiecesLength: 1000,
		PiecesCount:  4,
		Pieces: map[string]engine.PieceState{
			"0": {Completed: true, Length: 1000},
			"1": {Completed: true, Length: 1000},
			"2": {Completed: false, Length: 1000},
		},
	}
	f1, _ := engine.FileByIndex(st, 0)
	if n := engine.ContiguousForFile(st, cs, f1); n != 1000 {
		t.Fatalf("file 1 contiguous = %d, want 1000", n)
	}
	f2, _ := engine.FileByIndex(st, 1)
	// file 2 spans pieces 1..3; piece 2 is incomplete → runway stops there
	// even though ProgressForFile would count later complete pieces.
	if n := engine.ContiguousForFile(st, cs, f2); n != 1000 {
		t.Fatalf("file 2 contiguous = %d, want 1000", n)
	}
	if n := engine.ContiguousForFile(st, engine.CacheState{}, f1); n != 0 {
		t.Fatalf("empty cache contiguous = %d, want 0", n)
	}
	// A later complete piece without the preceding one is not runway.
	late := engine.CacheState{
		PiecesLength: 1000,
		Pieces: map[string]engine.PieceState{
			"2": {Completed: true, Length: 1000},
			"3": {Completed: true, Length: 1000},
		},
	}
	if n := engine.ContiguousForFile(st, late, f2); n != 0 {
		t.Fatalf("non-contiguous file 2 = %d, want 0", n)
	}
}

// TestRemAndList pins the engine actions the /v1 delete and list endpoints
// use: rem answers 200 with no body, list answers a JSON array.
func TestRemAndList(t *testing.T) {
	var remSeen, listSeen bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Action, Hash string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
			return
		}
		switch req.Action {
		case "rem":
			remSeen = true
			if req.Hash != "ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12" {
				t.Errorf("rem hash = %q", req.Hash)
			}
			w.WriteHeader(http.StatusOK)
		case "list":
			listSeen = true
			_ = json.NewEncoder(w).Encode([]engine.TorrentStatus{
				{Name: "multi", Hash: "ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12", Stat: 3},
			})
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	c := engine.NewClient(srv.URL)
	if err := c.Rem(context.Background(), "ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12"); err != nil {
		t.Fatalf("Rem: %v", err)
	}
	if !remSeen {
		t.Fatal("engine never saw action rem")
	}
	list, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !listSeen || len(list) != 1 || list[0].Name != "multi" {
		t.Fatalf("List = %+v", list)
	}
}

// TestListEmpty checks an engine with nothing loaded decodes to an empty
// slice rather than an error (the engine answers `[]`).
func TestListEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()
	list, err := engine.NewClient(srv.URL).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("List = %+v, want empty", list)
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
