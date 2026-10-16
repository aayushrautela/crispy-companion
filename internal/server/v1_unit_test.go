package server

import (
	"testing"
	"time"
)

// TestComputeReady pins the time-based readiness rule (spec §1.1): bytes
// alone decide while the bitrate is unmeasurable; once it is known the runway
// in seconds decides too.
func TestComputeReady(t *testing.T) {
	cfg := BufferConfig{
		MinBytes:                 4 << 20,
		StartSeconds:             10,
		BitrateWindowSeconds:     10,
		SustainableMarginPercent: 100,
	}
	const mib = int64(1 << 20)
	cases := []struct {
		name       string
		complete   bool
		contiguous int64
		bitrate    float64
		want       bool
	}{
		{"complete is always ready", true, 0, 0, true},
		{"below min bytes", false, 1 * mib, 0, false},
		{"min bytes met, bitrate unmeasured", false, 5 * mib, 0, true},
		{"min bytes met, runway too short", false, 5 * mib, 5 * float64(mib), false},
		{"min bytes met, runway long enough", false, 20 * mib, float64(mib), true},
	}
	for _, tc := range cases {
		if got := computeReady(cfg, tc.complete, tc.contiguous, tc.bitrate); got != tc.want {
			t.Errorf("%s: computeReady = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestObserveBitrate covers the windowed coverage-rate estimate: one sample
// is never enough, spans under a second are not measurable, growth reports
// bytes/sec and shrink/clamp reports zero.
func TestObserveBitrate(t *testing.T) {
	s := NewV1State(BufferConfig{}, false)
	t0 := time.Unix(1_700_000_000, 0)

	if got := s.observeBitrate("h", 1000, t0); got != 0 {
		t.Fatalf("first sample: %v, want 0", got)
	}
	if got := s.observeBitrate("h", 2000, t0.Add(500*time.Millisecond)); got != 0 {
		t.Fatalf("sub-second span: %v, want 0", got)
	}
	if got := s.observeBitrate("h", 4000, t0.Add(1500*time.Millisecond)); got != 2000 {
		t.Fatalf("1.5s span, 3000 new bytes: %v, want 2000 B/s", got)
	}
	if got := s.observeBitrate("h", 1000, t0.Add(2*time.Second)); got != 0 {
		t.Fatalf("shrinking coverage: %v, want 0", got)
	}
}
