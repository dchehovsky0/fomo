package alertinfo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"sync/atomic"
	"testing"
	"time"

	"fomobot/internal/dexscreener"
	"fomobot/internal/fomo"
	"fomobot/internal/launches"
	"fomobot/internal/notify"
)

var now = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

// fakeFomo behaves like sortedThesis: items filtered by the window, capped
// by limit and returned out of order; Count is the all-time total.
type fakeFomo struct {
	all   []fomo.Thesis
	calls atomic.Int64
}

func (f *fakeFomo) SortedThesis(_ context.Context, _ string, after, before time.Time, limit int) (*fomo.TokenThesisPage, error) {
	f.calls.Add(1)
	var in []fomo.Thesis
	for _, t := range f.all {
		if t.CreatedAt.After(after) && t.CreatedAt.Before(before) {
			in = append(in, t)
		}
	}
	rand.Shuffle(len(in), func(i, j int) { in[i], in[j] = in[j], in[i] })
	page := &fomo.TokenThesisPage{Count: len(f.all), HasNextPage: len(in) > limit}
	page.Items = in[:min(limit, len(in))]
	return page, nil
}

// theses returns n theses, one every step, the last one at now.
func theses(n int, step time.Duration) []fomo.Thesis {
	out := make([]fomo.Thesis, n)
	for i := range out {
		out[i] = fomo.Thesis{
			ID: string(rune('a' + i%26)), Handle: "user", Comment: "text",
			CreatedAt: now.Add(-time.Duration(n-1-i) * step), PositionUSD: float64(i + 1), MarketCapAtCreation: 1000 * float64(i+1),
		}
	}
	return out
}

type fakePairs struct {
	pairs []dexscreener.Pair
	err   error
}

func (p fakePairs) Pairs(context.Context, string) ([]dexscreener.Pair, error) { return p.pairs, p.err }

func builder(checkers []Checker, pairs PairSource, lookup LaunchLookup) *Builder {
	b := New(Config{
		FirstN: 3, RateWindow: 10 * time.Minute, NetworkID: fomo.SolanaNetworkID,
		FomoLinkTemplate:  "https://fomo.family/tokens/solana/{mint}",
		AxiomLinkTemplate: "https://axiom.trade/meme/{pair}?chain=sol",
	}, checkers, pairs, lookup, slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.now = func() time.Time { return now }
	return b
}

func positions(ts []notify.Thesis) []float64 {
	var out []float64
	for _, t := range ts {
		out = append(out, t.PositionUSD)
	}
	return out
}

func TestCompleteHintNeedsNoRequests(t *testing.T) {
	f := &fakeFomo{}
	hint := &fomo.TokenThesisPage{Count: 4, Items: theses(4, 5*time.Minute)}
	hint.Items[0], hint.Items[3] = hint.Items[3], hint.Items[0]
	lookup := func(string) (launches.Launch, bool) {
		return launches.Launch{CreatedAt: now.Add(-time.Hour), Dex: "pumpfun", Pool: "CURVE"}, true
	}
	a := builder([]Checker{f}, nil, lookup).Complete(context.Background(), notify.Alert{Token: "MINT", DetectedAt: now}, hint)

	if f.calls.Load() != 0 {
		t.Errorf("requests = %d, want 0", f.calls.Load())
	}
	if got := positions(a.First); len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 || !a.FirstExact {
		t.Errorf("first = %v exact=%v", got, a.FirstExact)
	}
	// theses at -15m, -10m, -5m, 0: the ones inside the last 10 minutes
	if !a.RateKnown || a.Recent != 3 || a.RecentCapped || a.Count != 4 {
		t.Errorf("rate: known=%v recent=%d capped=%v count=%d", a.RateKnown, a.Recent, a.RecentCapped, a.Count)
	}
	if a.Chain != "Solana" || a.Dex != "pumpfun" || a.CreatedAt.IsZero() || a.MarketCap != 4000 {
		t.Errorf("alert = %+v", a)
	}
	if a.FomoURL != "https://fomo.family/tokens/solana/MINT" || a.AxiomURL != "https://axiom.trade/meme/CURVE?chain=sol" {
		t.Errorf("links: %s %s", a.FomoURL, a.AxiomURL)
	}
}

func TestCountLagDoesNotShrinkTheAlert(t *testing.T) {
	f := &fakeFomo{}
	hint := &fomo.TokenThesisPage{Count: 2, Items: theses(3, time.Minute)}
	a := builder([]Checker{f}, nil, nil).Complete(context.Background(), notify.Alert{Token: "MINT", DetectedAt: now}, hint)
	if f.calls.Load() != 0 {
		t.Errorf("requests = %d, want 0", f.calls.Load())
	}
	if a.Count != 3 {
		t.Errorf("count = %d, want 3 from the items already in the response", a.Count)
	}
}

func TestBigTokenFindsFirstTheses(t *testing.T) {
	f := &fakeFomo{all: theses(5000, 20*time.Second)} // ~28 hours, 30 theses per 10 min
	a := builder([]Checker{f, f}, nil, nil).Complete(context.Background(), notify.Alert{Token: "MINT", DetectedAt: now}, nil)

	if got := positions(a.First); len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 || !a.FirstExact {
		t.Fatalf("first = %v exact=%v", got, a.FirstExact)
	}
	if a.Count != 5000 || !a.RateKnown || a.Recent != 30 || a.RecentCapped {
		t.Errorf("count=%d recent=%d capped=%v", a.Count, a.Recent, a.RecentCapped)
	}
	if n := f.calls.Load(); n > 45 {
		t.Errorf("requests = %d", n)
	}
}

type deadAccount struct{ calls atomic.Int64 }

func (d *deadAccount) SortedThesis(context.Context, string, time.Time, time.Time, int) (*fomo.TokenThesisPage, error) {
	d.calls.Add(1)
	return nil, fmt.Errorf("get access token: %w", fomo.ErrUnauthorized)
}

func TestDeadAccountIsSkipped(t *testing.T) {
	dead, f := &deadAccount{}, &fakeFomo{all: theses(4, time.Minute)}
	a := builder([]Checker{dead, f}, nil, nil).Complete(context.Background(), notify.Alert{Token: "MINT", DetectedAt: now}, nil)
	if len(a.First) != 3 || !a.FirstExact || dead.calls.Load() == 0 {
		t.Errorf("first=%d exact=%v dead calls=%d", len(a.First), a.FirstExact, dead.calls.Load())
	}
}

func TestRateIsCapped(t *testing.T) {
	f := &fakeFomo{all: theses(3000, 100*time.Millisecond)}
	a := builder([]Checker{f}, nil, nil).Complete(context.Background(), notify.Alert{Token: "MINT", DetectedAt: now}, nil)
	if a.Recent != 500 || !a.RecentCapped {
		t.Errorf("recent=%d capped=%v", a.Recent, a.RecentCapped)
	}
}

func TestMarketFromDexScreener(t *testing.T) {
	pairs := fakePairs{pairs: []dexscreener.Pair{
		{PairAddress: "CURVE", LiquidityUSD: 10, MarketCap: 1, TokenName: "Cat", TokenSymbol: "CAT"},
		{PairAddress: "POOL", LiquidityUSD: 90_000, MarketCap: 2_500_000},
	}}
	a := builder(nil, pairs, nil).Complete(context.Background(), notify.Alert{Token: "MINT"}, nil)
	if a.AxiomURL != "https://axiom.trade/meme/POOL?chain=sol" || a.MarketCap != 2_500_000 || a.Name != "Cat" || a.Symbol != "CAT" {
		t.Errorf("alert = %+v", a)
	}

	a = builder(nil, pairs, nil).Complete(context.Background(), notify.Alert{Token: "MINT", MarketCap: 7, Symbol: "WS"}, nil)
	if a.MarketCap != 7 || a.Symbol != "WS" {
		t.Errorf("stream values must win: %+v", a)
	}

	a = builder(nil, fakePairs{err: errors.New("down")}, nil).Complete(context.Background(), notify.Alert{Token: "MINT"}, nil)
	if a.AxiomURL != "https://axiom.trade/meme/MINT?chain=sol" {
		t.Errorf("fallback link = %s", a.AxiomURL)
	}
}
