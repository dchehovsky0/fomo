package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSignalPersistedImmediately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	if err := s.MarkSignaled(Signal{Token: "OLD", Count: 3, SentAt: now.Add(-72 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkSignaled(Signal{Token: "T", Ticker: "TKN", Count: 3, SentAt: now}); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.IsSignaled("T") {
		t.Fatal("signal not persisted")
	}
	if n := reopened.PruneSignals(now.Add(-48 * time.Hour)); n != 1 || reopened.IsSignaled("OLD") || reopened.Signals() != 1 {
		t.Errorf("pruned %d, OLD signaled=%v", n, reopened.IsSignaled("OLD"))
	}
}

func TestTrendingState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := Open(path)
	now := time.Now().Truncate(time.Second)
	s.TouchTrending("SEEN", now)
	if err := s.SetTrendingBaselined(); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkTrendingAlerted("NEW", now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	s.TouchTrending("SEEN", now.Add(10*time.Second)) // throttled: under a minute

	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !r.TrendingBaselined() || r.TrendingTokens() != 2 {
		t.Fatalf("baselined=%v tokens=%d", r.TrendingBaselined(), r.TrendingTokens())
	}
	if at, _ := r.TrendingLastSeen("SEEN"); !at.Equal(now) {
		t.Errorf("SEEN last seen = %v, want %v", at, now)
	}
	if n := r.PruneTrending(now.Add(-24 * time.Hour)); n != 1 {
		t.Errorf("pruned %d, want 1", n)
	}
	if _, ok := r.TrendingLastSeen("NEW"); ok {
		t.Error("NEW must be pruned")
	}
}

func TestOpenOldStateFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	old := `{"seen":{"e1":"2026-09-27T10:00:00Z"},"signals":{"T":{"token":"T","count":3,"sent_at":"2026-09-27T10:00:00Z","silent":true}},"cursor":"2026-09-27T10:00:00Z","last_counts":{}}`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s.IsSignaled("T") {
		t.Error("signal from an old state file lost")
	}
}
