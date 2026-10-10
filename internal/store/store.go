package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Signal struct {
	Token  string `json:"token"`
	Ticker string `json:"ticker,omitempty"`
	Count  int    `json:"count"`
	// Threshold is the rung we already sent: 3, then 6, 12, 24.
	// Older rows leave it empty and only store Count.
	Threshold int       `json:"threshold,omitempty"`
	SentAt    time.Time `json:"sent_at"`
}

type state struct {
	Signals map[string]Signal `json:"signals"`
	// Trending: mint -> last time it was seen in fomo's trending list.
	Trending map[string]time.Time `json:"trending,omitempty"`
	// TrendingBaselined: the trending list present at the first start has
	// been recorded.
	TrendingBaselined bool `json:"trending_baselined,omitempty"`
}

// Store keeps sent signals and trending sightings in memory and dumps them to
// a JSON file. An empty path gives a purely in-memory store.
type Store struct {
	mu    sync.Mutex
	path  string
	st    state
	dirty bool
}

func Open(path string) (*Store, error) {
	s := &Store{path: path, st: state{Signals: map[string]Signal{}, Trending: map[string]time.Time{}}}
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read store: %w", err)
	}
	if err := json.Unmarshal(data, &s.st); err != nil {
		return nil, fmt.Errorf("parse store %s: %w", path, err)
	}
	if s.st.Signals == nil {
		s.st.Signals = map[string]Signal{}
	}
	if s.st.Trending == nil {
		s.st.Trending = map[string]time.Time{}
	}
	return s, nil
}

func (s *Store) TrendingBaselined() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.TrendingBaselined
}

func (s *Store) SetTrendingBaselined() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.TrendingBaselined = true
	s.dirty = true
	return s.flushLocked()
}

func (s *Store) TrendingLastSeen(mint string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.st.Trending[mint]
	return at, ok
}

// TouchTrending records a sighting in memory; it reaches the file with the
// next periodic flush.
func (s *Store) TouchTrending(mint string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.st.Trending[mint]; ok && at.Sub(cur) < time.Minute {
		return
	}
	s.st.Trending[mint] = at
	s.dirty = true
}

// MarkTrendingAlerted persists immediately, like MarkSignaled.
func (s *Store) MarkTrendingAlerted(mint string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Trending[mint] = at
	s.dirty = true
	return s.flushLocked()
}

func (s *Store) TrendingTokens() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.st.Trending)
}

// PruneTrending forgets tokens not seen in trending since before.
func (s *Store) PruneTrending(before time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for m, at := range s.st.Trending {
		if at.Before(before) {
			delete(s.st.Trending, m)
			n++
		}
	}
	if n > 0 {
		s.dirty = true
	}
	return n
}

func (s *Store) IsSignaled(token string) bool {
	_, ok := s.Signal(token)
	return ok
}

// Signal is the last alert stored for token.
func (s *Store) Signal(token string) (Signal, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sig, ok := s.st.Signals[token]
	return sig, ok
}

// MarkSignaled persists immediately: losing it in a crash means a duplicate alert.
func (s *Store) MarkSignaled(sig Signal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Signals[sig.Token] = sig
	s.dirty = true
	return s.flushLocked()
}

func (s *Store) Signals() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.st.Signals)
}

// PruneSignals drops signals sent before the given time: their tokens are too
// old to be watched again.
func (s *Store) PruneSignals(before time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for tok, sig := range s.st.Signals {
		if sig.SentAt.Before(before) {
			delete(s.st.Signals, tok)
			n++
		}
	}
	if n > 0 {
		s.dirty = true
	}
	return n
}

func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked()
}

func (s *Store) flushLocked() error {
	if !s.dirty || s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

// Run periodically prunes old entries and flushes the state until ctx ends,
// then flushes one last time. A zero TTL keeps entries forever.
func (s *Store) Run(ctx context.Context, every, signalTTL, trendingTTL time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := s.Flush(); err != nil {
				log.Error("final store flush failed", "err", err)
			}
			return
		case <-t.C:
			started := time.Now()
			var prunedSignals, prunedTrending int
			if signalTTL > 0 {
				prunedSignals = s.PruneSignals(time.Now().Add(-signalTTL))
			}
			if trendingTTL > 0 {
				prunedTrending = s.PruneTrending(time.Now().Add(-trendingTTL))
			}
			if err := s.Flush(); err != nil {
				log.Error("store flush failed", "took", time.Since(started).Round(time.Millisecond), "err", err)
				continue
			}
			took := time.Since(started)
			if prunedSignals > 0 || prunedTrending > 0 || took > 50*time.Millisecond {
				log.Info("store flush", "took", took.Round(time.Millisecond),
					"pruned_signals", prunedSignals, "pruned_trending", prunedTrending,
					"signals", s.Signals(), "trending", s.TrendingTokens())
			}
		}
	}
}
