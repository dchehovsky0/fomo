package launches

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"fomobot/internal/axiom"
)

type fakeStream []axiom.Pair

func (f fakeStream) Run(_ context.Context, handle func(axiom.Pair)) {
	for _, p := range f {
		handle(p)
	}
}

func pair(mint, protocol string, at time.Time) axiom.Pair {
	return axiom.Pair{Token: mint, Pair: "pool-" + mint, Ticker: "T" + mint, Name: "Name " + mint,
		Protocol: protocol, DisplayProtocol: protocol, CreatedAt: at}
}

func TestRunDedupsAndFilters(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	stream := fakeStream{
		pair("A", "Pump V1", now.Add(-time.Second)),
		pair("B", "virtual curve", now),
		pair("A", "Pump V1", now),                     // second pool of the same mint
		pair("C", "Moonshot", now),                    // protocol not listed
		pair("OLD", "Pump V1", now.Add(-7*time.Hour)), // older than the lifetime
		{Token: "D", Protocol: "x", DisplayProtocol: "Pump V1"},
		pair("E", "Pump V1", now.Add(time.Hour)), // clock skew
	}
	s := New(stream, Config{Protocols: []string{"Pump V1", "Virtual Curve"}, Lifetime: 6 * time.Hour},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.now = func() time.Time { return now }

	var got []Launch
	s.Run(context.Background(), func(l Launch) { got = append(got, l) })

	mints := make([]string, len(got))
	for i, l := range got {
		mints[i] = l.Mint
	}
	if want := []string{"A", "B", "D", "E"}; !slices.Equal(mints, want) {
		t.Fatalf("emitted %v, want %v", mints, want)
	}
	if a := got[0]; a.Dex != "Pump V1" || a.Pool != "pool-A" || a.Symbol != "TA" || a.Name != "Name A" || !a.CreatedAt.Equal(now.Add(-time.Second)) {
		t.Errorf("A = %+v", a)
	}
	if !got[2].CreatedAt.Equal(now) || !got[3].CreatedAt.Equal(now) {
		t.Errorf("missing or future created_at must become now: %v %v", got[2].CreatedAt, got[3].CreatedAt)
	}
	if s.Stats.Duplicates.Load() != 1 || s.Stats.Filtered.Load() != 1 || s.Stats.Old.Load() != 1 || s.Stats.Received.Load() != 7 {
		t.Errorf("dup=%d filtered=%d old=%d received=%d", s.Stats.Duplicates.Load(), s.Stats.Filtered.Load(), s.Stats.Old.Load(), s.Stats.Received.Load())
	}
	if _, ok := s.Lookup("A"); !ok {
		t.Error("lookup A")
	}
	if got := s.Protocols(); got != "Pump V1=5 Moonshot=1 virtual curve=1" {
		t.Errorf("protocols = %q", got)
	}

	s.now = func() time.Time { return now.Add(8 * time.Hour) }
	s.prune()
	if _, ok := s.Lookup("A"); ok {
		t.Error("old mints must be pruned")
	}
}

func TestEmptyProtocolsTakesAll(t *testing.T) {
	now := time.Now()
	s := New(fakeStream{pair("A", "Pump V1", now), pair("B", "Moonshot", now)}, Config{Lifetime: time.Hour},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	n := 0
	s.Run(context.Background(), func(Launch) { n++ })
	if n != 2 {
		t.Errorf("emitted %d, want 2", n)
	}
}

func TestSkippedProtocolNeverEnters(t *testing.T) {
	now := time.Now()
	s := New(fakeStream{
		pair("M", "Meteora AMM V2", now),
		pair("M", "Pump V1", now), // a later pool must not let the mint in
		pair("P", "Pump V1", now),
	}, Config{SkipProtocols: []string{"Meteora AMM V2"}, Lifetime: time.Hour},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	var got []string
	s.Run(context.Background(), func(l Launch) { got = append(got, l.Mint) })
	if !slices.Equal(got, []string{"P"}) {
		t.Fatalf("emitted %v", got)
	}
	if !s.Blocked("M") || s.Blocked("P") {
		t.Fatalf("blocked M=%v P=%v", s.Blocked("M"), s.Blocked("P"))
	}
}

func TestPumpV1MigratesToPumpAMM(t *testing.T) {
	now := time.Date(2026, 10, 7, 19, 24, 36, 0, time.UTC)
	curve := pair("R", "Pump V1", now)
	curve.Pair = "curve"
	meteora := pair("R", "Meteora AMM V2", now.Add(time.Minute))
	meteora.Pair = "meteora"
	amm := pair("R", "Pump AMM", now.Add(2*time.Minute))
	amm.Pair = "amm"
	again := amm
	again.Pair = "amm-2"
	other := pair("Q", "Pump AMM", now)
	stream := fakeStream{curve, meteora, amm, again, other}
	s := New(stream, Config{
		Protocols: []string{"Pump V1"}, SkipProtocols: []string{"Meteora AMM V2"}, Lifetime: time.Hour,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.now = func() time.Time { return now.Add(3 * time.Minute) }
	var migrated []Launch
	s.OnMigrate = func(l Launch) { migrated = append(migrated, l) }
	var got []Launch
	s.Run(context.Background(), func(l Launch) { got = append(got, l) })

	if len(got) != 1 || got[0].Mint != "R" || got[0].Pool != "curve" || got[0].Dex != "Pump V1" {
		t.Fatalf("emitted %+v", got)
	}
	if len(migrated) != 1 || migrated[0].Pool != "amm" || migrated[0].Dex != "Pump AMM" || !migrated[0].CreatedAt.Equal(now) {
		t.Fatalf("migrated %+v", migrated)
	}
	look, ok := s.Lookup("R")
	if !ok || look.Pool != "amm" || look.Dex != "Pump AMM" {
		t.Fatalf("lookup %+v %v", look, ok)
	}
	if s.Stats.Migrated.Load() != 1 || s.Stats.Duplicates.Load() != 1 || s.Stats.Emitted.Load() != 1 || s.Stats.Filtered.Load() != 2 {
		t.Fatalf("migrated=%d dup=%d emitted=%d filtered=%d",
			s.Stats.Migrated.Load(), s.Stats.Duplicates.Load(), s.Stats.Emitted.Load(), s.Stats.Filtered.Load())
	}
}
