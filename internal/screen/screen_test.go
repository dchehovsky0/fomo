package screen

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"fomobot/internal/dexscreener"
	"fomobot/internal/domain"
)

type fakeQ struct {
	limit  int
	quotes map[string]dexscreener.Quote
	failed []string
	calls  int
}

func (f *fakeQ) Limit() int { return f.limit }
func (f *fakeQ) Wave(context.Context, []string) (map[string]dexscreener.Quote, []string, error) {
	f.calls++
	return f.quotes, f.failed, nil
}

func TestGateFiltersByMarketCap(t *testing.T) {
	q := &fakeQ{limit: 300, quotes: map[string]dexscreener.Quote{
		"live": {MarketCap: 8000},
		"dead": {MarketCap: 4000},
	}}
	g := New(Config{After: 10 * time.Second, MinMarketCap: 7000, Retry: time.Second}, q, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Now()
	g.now = func() time.Time { return now }
	g.Add(domain.Token{Mint: "live", Symbol: "L", CreatedAt: now.Add(-time.Minute)})
	g.Add(domain.Token{Mint: "dead", Symbol: "D", CreatedAt: now.Add(-time.Minute)})
	g.Add(domain.Token{Mint: "new", Symbol: "N", CreatedAt: now})

	var got []string
	g.wave(context.Background(), func(token domain.Token) { got = append(got, token.Mint) })
	if len(got) != 1 || got[0] != "live" {
		t.Fatalf("emitted %v", got)
	}
	g.mu.Lock()
	if _, ok := g.pending["dead"]; ok {
		t.Fatal("dead token stayed pending")
	}
	if _, ok := g.pending["new"]; !ok {
		t.Fatal("young token was checked too soon")
	}
	g.mu.Unlock()

	q.quotes = map[string]dexscreener.Quote{}
	now = now.Add(time.Second)
	g.wave(context.Background(), func(domain.Token) { t.Fatal("unknown cap must be retried, not emitted") })
	g.mu.Lock()
	if _, ok := g.pending["live"]; ok {
		t.Fatal("live token was put back")
	}
	_, still := g.pending["new"]
	g.mu.Unlock()
	if !still {
		t.Fatal("token with no cap was dropped")
	}
}

func TestVolumeDoesNotBlockEntry(t *testing.T) {
	q := &fakeQ{limit: 10, quotes: map[string]dexscreener.Quote{
		"loud":  {MarketCap: 8000, VolumeUSD: 1, VolumeKnown: true},
		"quiet": {MarketCap: 8000},
		"small": {MarketCap: 5000, VolumeUSD: 20000, VolumeKnown: true},
	}}
	g := New(Config{After: 10 * time.Second, MinMarketCap: 7000, Retry: time.Second}, q, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Now()
	g.now = func() time.Time { return now }
	for _, mint := range []string{"loud", "quiet", "small"} {
		g.Add(domain.Token{Mint: mint, CreatedAt: now.Add(-time.Minute)})
	}
	var got []string
	g.wave(context.Background(), func(token domain.Token) { got = append(got, token.Mint) })
	if len(got) != 2 {
		t.Fatalf("emitted %v, want loud and quiet", got)
	}
}

func TestMissingCapIsRetried(t *testing.T) {
	q := &fakeQ{limit: 10, quotes: map[string]dexscreener.Quote{
		"cheap": {MarketCap: 4000},
	}}
	g := New(Config{After: 10 * time.Second, MinMarketCap: 7000, Retry: time.Second}, q, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Now()
	g.now = func() time.Time { return now }
	g.Add(domain.Token{Mint: "cheap", CreatedAt: now.Add(-time.Minute)})
	g.Add(domain.Token{Mint: "gone", CreatedAt: now.Add(-time.Minute)})
	var got []string
	g.wave(context.Background(), func(token domain.Token) { got = append(got, token.Mint) })
	if len(got) != 0 {
		t.Fatalf("first wave emitted %v", got)
	}
	now = now.Add(time.Second)
	g.wave(context.Background(), func(domain.Token) { t.Fatal("token missing from dexscreener must stay pending") })
	g.mu.Lock()
	_, still := g.pending["gone"]
	g.mu.Unlock()
	if !still {
		t.Fatal("missing token was dropped")
	}
}

func TestDexHealthSaysWhichRequestReturnedCap(t *testing.T) {
	q := &fakeQ{limit: 10, quotes: map[string]dexscreener.Quote{}}
	g := New(Config{After: 10 * time.Second, MinMarketCap: 7000, Retry: time.Second}, q, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Now()
	g.now = func() time.Time { return now }
	g.Add(domain.Token{Mint: "late", Symbol: "L", CreatedAt: now.Add(-time.Minute)})
	g.Add(domain.Token{Mint: "now", Symbol: "N", CreatedAt: now.Add(-time.Minute)})
	g.wave(context.Background(), func(domain.Token) { t.Fatal("nothing listed yet") })
	q.quotes = map[string]dexscreener.Quote{
		"late": {MarketCap: 12000, VolumeUSD: 8000, VolumeKnown: true},
	}
	now = now.Add(time.Second)
	var got []string
	g.wave(context.Background(), func(token domain.Token) { got = append(got, token.Mint) })
	if len(got) != 1 || got[0] != "late" {
		t.Fatalf("emitted %v", got)
	}
	h := g.Health()
	if h.Tokens != 2 || h.Requests != 4 || h.CapFromRequest[2] != 1 || h.VolumeFromRequest[2] != 1 || h.Passed != 1 || h.Dropped != 0 || h.Waiting != 1 {
		t.Fatalf("health = %+v", h)
	}
	byMint := map[string]DexToken{}
	for _, tok := range h.Recent {
		byMint[tok.Mint] = tok
	}
	nowTok, lateTok := byMint["now"], byMint["late"]
	if nowTok.Decision != "" || len(nowTok.Attempts) != 2 || nowTok.Attempts[0].N != 1 || nowTok.Attempts[1].CapKnown {
		t.Fatalf("now = %+v", nowTok)
	}
	if lateTok.Decision != "в работу" || !lateTok.Attempts[1].CapKnown || lateTok.Attempts[1].VolumeUSD != 8000 || lateTok.Attempts[1].N != 2 {
		t.Fatalf("late = %+v", lateTok)
	}
}

type memBook struct {
	tokens []domain.Token
}

func (b *memBook) Tokens() []domain.Token       { return b.VolumeTokens() }
func (b *memBook) VolumeTokens() []domain.Token { return append([]domain.Token(nil), b.tokens...) }
func (b *memBook) Release(mint string) bool {
	for i, tok := range b.tokens {
		if tok.Mint == mint {
			b.tokens = append(b.tokens[:i], b.tokens[i+1:]...)
			return true
		}
	}
	return false
}

func TestVolumeNeedsTwoFlatSamples(t *testing.T) {
	q := &fakeQ{limit: 10, quotes: map[string]dexscreener.Quote{
		"flat":  {VolumeUSD: 100, VolumeKnown: true},
		"alive": {VolumeUSD: 100, VolumeKnown: true},
		"blank": {},
	}}
	g := New(Config{MinMarketCap: 7000, MinGrowth: 0.05, Recheck: time.Hour}, q, slog.New(slog.NewTextHandler(io.Discard, nil)))
	book := &memBook{tokens: []domain.Token{
		{Mint: "flat", Symbol: "F"},
		{Mint: "alive", Symbol: "A"},
		{Mint: "blank", Symbol: "B"},
		{Mint: "down", Symbol: "D"},
	}}
	q.failed = []string{"down"}
	g.recheck(context.Background(), book)
	if len(book.tokens) != 4 {
		t.Fatalf("baseline dropped tokens: %v", book.tokens)
	}

	q.quotes["flat"] = dexscreener.Quote{VolumeUSD: 104, VolumeKnown: true}
	q.quotes["alive"] = dexscreener.Quote{VolumeUSD: 105, VolumeKnown: true}
	g.recheck(context.Background(), book)
	if !book.has("flat") || !book.has("alive") || !book.has("blank") || !book.has("down") {
		t.Fatalf("one bad sample dropped tokens: %v", book.tokens)
	}

	q.quotes["flat"] = dexscreener.Quote{VolumeUSD: 104, VolumeKnown: true}
	q.quotes["alive"] = dexscreener.Quote{VolumeUSD: 200, VolumeKnown: true}
	g.recheck(context.Background(), book)
	if book.has("flat") || !book.has("alive") || !book.has("blank") || !book.has("down") {
		t.Fatalf("left = %v", book.tokens)
	}
}

func (b *memBook) has(mint string) bool {
	for _, tok := range b.tokens {
		if tok.Mint == mint {
			return true
		}
	}
	return false
}
