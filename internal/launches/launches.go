// Package launches turns Axiom's stream of new pairs into new tokens: one
// launch per mint, only the configured protocols, nothing older than the
// lifetime. The stream is real time only: tokens launched while the bot is
// down are not read later.
package launches

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fomobot/internal/axiom"
	"fomobot/internal/domain"
)

// Launch is a new token. The type lives in domain.
type Launch = domain.Token

type Stream interface {
	Run(ctx context.Context, handle func(axiom.Pair))
}

type Config struct {
	// Protocols: only pairs whose protocol or display_protocol is listed,
	// case-insensitive; empty takes every protocol.
	Protocols []string
	// SkipProtocols are dropped before anything else. A mint first seen on
	// one of them is remembered and never emitted, even from a later pool.
	SkipProtocols []string
	// Lifetime: tokens older than this are not emitted.
	Lifetime time.Duration
}

type Stats struct {
	Received   atomic.Int64
	Emitted    atomic.Int64
	Duplicates atomic.Int64 // another pool of a mint already seen
	Filtered   atomic.Int64 // protocol not in the list
	Old        atomic.Int64 // created longer than the lifetime ago
}

type Source struct {
	stream    Stream
	cfg       Config
	log       *slog.Logger
	now       func() time.Time
	protocols map[string]bool
	skip      map[string]bool
	Stats     Stats

	mu      sync.Mutex
	seen    map[string]Launch
	byProto map[string]int64
}

func New(stream Stream, cfg Config, log *slog.Logger) *Source {
	return &Source{
		stream: stream, cfg: cfg, log: log, now: time.Now,
		protocols: names(cfg.Protocols), skip: names(cfg.SkipProtocols),
		seen: map[string]Launch{}, byProto: map[string]int64{},
	}
}

func names(list []string) map[string]bool {
	out := map[string]bool{}
	for _, p := range list {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			out[p] = true
		}
	}
	return out
}

// Skips reports whether this protocol name is on the deny list.
func (s *Source) Skips(name string) bool {
	return s.skip[strings.ToLower(strings.TrimSpace(name))]
}

// Blocked reports whether mint was first seen on a skipped protocol.
func (s *Source) Blocked(mint string) bool {
	l, ok := s.Lookup(mint)
	return ok && s.Skips(l.Dex)
}

// Lookup returns the launch of a mint seen recently (about lifetime + 1h).
func (s *Source) Lookup(mint string) (Launch, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.seen[mint]
	return l, ok
}

// Run calls emit once per new mint until ctx is done.
func (s *Source) Run(ctx context.Context, emit func(Launch)) {
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.prune()
			}
		}
	}()
	s.stream.Run(ctx, func(p axiom.Pair) { s.handle(p, emit) })
}

func (s *Source) handle(p axiom.Pair, emit func(Launch)) bool {
	s.Stats.Received.Add(1)
	now := s.now()
	label := p.Label()
	s.mu.Lock()
	s.byProto[label]++
	s.mu.Unlock()
	if s.Skips(p.Protocol) || s.Skips(p.DisplayProtocol) || s.Skips(label) {
		s.Stats.Filtered.Add(1)
		s.mu.Lock()
		if _, ok := s.seen[p.Token]; !ok {
			s.seen[p.Token] = launchFrom(p, time.Time{})
		}
		s.mu.Unlock()
		s.log.Info("flow", "step", "обработали", "decision", "пропуск", "why", "протокол исключён",
			"token", p.Token, "ticker", p.Ticker, "protocol", label)
		return false
	}
	if len(s.protocols) > 0 && !s.protocols[strings.ToLower(p.Protocol)] && !s.protocols[strings.ToLower(p.DisplayProtocol)] {
		s.Stats.Filtered.Add(1)
		s.log.Info("flow", "step", "обработали", "decision", "пропуск", "why", "протокол не в списке",
			"token", p.Token, "ticker", p.Ticker, "protocol", label)
		return false
	}
	at := p.CreatedAt
	if at.IsZero() || at.After(now) {
		at = now
	}

	s.mu.Lock()
	if _, ok := s.seen[p.Token]; ok {
		s.mu.Unlock()
		s.Stats.Duplicates.Add(1)
		s.log.Info("flow", "step", "обработали", "decision", "пропуск", "why", "этот токен уже видели",
			"token", p.Token, "ticker", p.Ticker, "protocol", label)
		return false
	}
	l := launchFrom(p, at)
	s.seen[p.Token] = l
	s.mu.Unlock()

	age := now.Sub(at).Round(time.Second)
	if s.cfg.Lifetime > 0 && now.Sub(at) > s.cfg.Lifetime {
		s.Stats.Old.Add(1)
		s.log.Info("flow", "step", "обработали", "decision", "пропуск", "why", "токен старше срока наблюдения",
			"token", p.Token, "ticker", p.Ticker, "protocol", label, "age", age)
		return false
	}
	s.Stats.Emitted.Add(1)
	s.log.Info("flow", "step", "обработали", "decision", "в работу",
		"token", p.Token, "ticker", p.Ticker, "name", p.Name, "protocol", label, "age", age, "pair", p.Pair)
	emit(l)
	return true
}

func launchFrom(p axiom.Pair, at time.Time) Launch {
	return Launch{
		Mint: p.Token, CreatedAt: at, Dex: p.Label(), Pool: p.Pair, Symbol: p.Ticker, Name: p.Name,
		Deployer: p.Deployer, Website: p.Website, Twitter: p.Twitter, Telegram: p.Telegram, Discord: p.Discord,
		ImageURL: domain.PictureURL(p.ImageURL), LiquiditySOL: p.LiquiditySOL,
	}
}

// Protocols lists pairs received per protocol since the start, most first,
// e.g. "Pump V1=812 Virtual Curve=40".
func (s *Source) Protocols() string {
	s.mu.Lock()
	type kv struct {
		name string
		n    int64
	}
	all := make([]kv, 0, len(s.byProto))
	for name, n := range s.byProto {
		all = append(all, kv{name, n})
	}
	s.mu.Unlock()
	slices.SortFunc(all, func(a, b kv) int { return cmp.Or(cmp.Compare(b.n, a.n), cmp.Compare(a.name, b.name)) })
	parts := make([]string, len(all))
	for i, e := range all {
		parts[i] = fmt.Sprintf("%s=%d", e.name, e.n)
	}
	return strings.Join(parts, " ")
}

// prune forgets mints too old to be emitted or looked up.
func (s *Source) prune() {
	limit := s.now().Add(-s.cfg.Lifetime - time.Hour)
	s.mu.Lock()
	defer s.mu.Unlock()
	for m, l := range s.seen {
		if l.CreatedAt.Before(limit) {
			delete(s.seen, m)
		}
	}
}
