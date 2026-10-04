package trending

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fomobot/internal/fomo"
	"fomobot/internal/notify"
	"fomobot/internal/store"
)

const sol = "1399811149"

func item(mint, network, symbol string) string {
	return fmt.Sprintf(`{"change24":"258.46","holders":4233,"marketCap":"1256554.89","priceUSD":"0.0013","volume24":"3667862.67",`+
		`"token":{"address":%q,"info":{"id":"%s:%s"},"launchpad":{"launchpadName":"Pump.fun"},"name":"cat wif sword","networkId":%s,"symbol":%q}}`,
		mint, mint, network, network, symbol)
}

func snapshot(items ...string) []byte {
	list := ""
	for i, it := range items {
		if i > 0 {
			list += ","
		}
		list += it
	}
	return []byte(`{"type":"data","topicType":"trending_tokens","topicId":"1,56","payload":{"kind":"snapshot","tokens":[` + list + `]}}`)
}

func update(index int, it string) []byte {
	return []byte(fmt.Sprintf(`{"type":"data","topicType":"trending_tokens","payload":{"kind":"update","index":%d,"tokenKey":"x","update":%s}}`, index, it))
}

func TestParse(t *testing.T) {
	msg, ok := Parse(snapshot(item("0xabc", "56", "BNB1"), item("MINT1", sol, "swordcat")))
	if !ok || msg.Kind != KindSnapshot || len(msg.Tokens) != 2 {
		t.Fatalf("snapshot = %+v, %v", msg, ok)
	}
	tok := msg.Tokens[1]
	if tok.Mint != "MINT1" || tok.NetworkID != sol || tok.Symbol != "swordcat" || tok.Name != "cat wif sword" ||
		tok.Launchpad != "Pump.fun" || tok.Rank != 2 || tok.Holders != 4233 || tok.MarketCap != 1256554.89 || tok.Change24 != 258.46 {
		t.Errorf("token = %+v", tok)
	}

	msg, ok = Parse(update(4, item("MINT2", sol, "B")))
	if !ok || msg.Kind != KindUpdate || len(msg.Tokens) != 1 || msg.Tokens[0].Rank != 5 {
		t.Errorf("update = %+v, %v", msg, ok)
	}

	msg, ok = Parse([]byte(`{"type":"data","topicType":"trending_tokens","payload":{"kind":"remove","tokenKey":"MINT3:1399811149"}}`))
	if !ok || msg.Kind != KindRemove || msg.Removed != "MINT3" || msg.RemovedNetwork != sol {
		t.Errorf("remove = %+v, %v", msg, ok)
	}

	for _, raw := range []string{`{"type":"challenge"}`, `{"type":"subscribed","topicType":"trending_tokens"}`, `not json`,
		`{"type":"data","topicType":"prices","payload":{"kind":"update","update":{"token":{"address":"X"}}}}`} {
		if _, ok := Parse([]byte(raw)); ok {
			t.Errorf("Parse(%s) must be ignored", raw)
		}
	}
}

type fakeNotifier struct {
	mu      sync.Mutex
	sent    []notify.Alert
	failing atomic.Int64
}

func (n *fakeNotifier) Send(_ context.Context, s notify.Alert) error {
	if n.failing.Add(-1) >= 0 {
		return errors.New("telegram down")
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, s)
	return nil
}

func (n *fakeNotifier) tokens() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for _, s := range n.sent {
		out = append(out, s.Token)
	}
	return out
}

func newAlerter(t *testing.T, cfg Config, n *fakeNotifier) (*Alerter, *store.Store) {
	t.Helper()
	st, _ := store.Open("")
	cfg.NetworkID = sol
	cfg.RetryDelay = 10 * time.Millisecond
	cfg.Complete = func(_ context.Context, a notify.Alert, _ *fomo.TokenThesisPage) notify.Alert {
		a.CountKnown, a.Count, a.FomoURL = true, 2, "https://fomo.family/tokens/solana/"+a.Token
		return a
	}
	a := NewAlerter(cfg, st, n, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return a, st
}

func handle(a *Alerter, raw []byte) {
	msg, ok := Parse(raw)
	if !ok {
		panic("bad test message")
	}
	a.Handle(msg)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFirstSnapshotIsBaselineThenNewTokensAlert(t *testing.T) {
	n := &fakeNotifier{}
	a, st := newAlerter(t, Config{SilentFirstSnapshot: true, RealertAfter: time.Hour}, n)

	handle(a, update(0, item("EARLY", sol, "E"))) // before the first snapshot: ignored
	handle(a, snapshot(item("OLD1", sol, "O1"), item("0xbnb", "56", "BNB"), item("OLD2", sol, "O2")))
	if !st.TrendingBaselined() || st.TrendingTokens() != 2 || a.Stats.Baseline.Load() != 2 {
		t.Fatalf("baseline: done=%v tokens=%d", st.TrendingBaselined(), st.TrendingTokens())
	}

	handle(a, snapshot(item("OLD1", sol, "O1"), item("YOUNG", sol, "Y"), item("OLD2", sol, "O2")))
	handle(a, update(1, item("YOUNG", sol, "Y")))  // the same token again while pending or sent
	handle(a, update(9, item("0xnew", "56", "N"))) // other chain
	waitFor(t, "alert", func() bool { return len(n.tokens()) == 1 })
	time.Sleep(30 * time.Millisecond)
	if got := n.tokens(); len(got) != 1 || got[0] != "YOUNG" {
		t.Fatalf("sent %v, want only YOUNG", got)
	}
	n.mu.Lock()
	s := n.sent[0]
	n.mu.Unlock()
	if s.Kind != notify.KindTrending || s.Rank != 2 || s.Symbol != "Y" || s.Name != "cat wif sword" || s.MarketCap != 1256554.89 || s.Count != 2 || s.FomoURL == "" || s.Returned {
		t.Errorf("signal = %+v", s)
	}
	if _, ok := st.TrendingLastSeen("YOUNG"); !ok {
		t.Error("alerted token must be stored")
	}
}

func TestReturnAfterRealertWindow(t *testing.T) {
	n := &fakeNotifier{}
	a, st := newAlerter(t, Config{RealertAfter: time.Hour}, n)
	st.TouchTrending("BACK", time.Now().Add(-2*time.Hour))
	st.TouchTrending("STAY", time.Now().Add(-10*time.Minute))

	handle(a, snapshot(item("BACK", sol, "B"), item("STAY", sol, "S")))
	waitFor(t, "alert", func() bool { return len(n.tokens()) == 1 })
	n.mu.Lock()
	s := n.sent[0]
	n.mu.Unlock()
	if s.Token != "BACK" || !s.Returned {
		t.Errorf("signal = %+v, want BACK returned", s)
	}
}

func TestSendFailures(t *testing.T) {
	n := &fakeNotifier{}
	n.failing.Store(2)
	a, st := newAlerter(t, Config{MaxAttempts: 3}, n)
	handle(a, snapshot(item("A", sol, "A")))
	waitFor(t, "alert after retries", func() bool { return len(n.tokens()) == 1 })
	if a.Stats.SendErrors.Load() != 2 {
		t.Errorf("send errors = %d", a.Stats.SendErrors.Load())
	}

	n.failing.Store(100)
	handle(a, snapshot(item("A", sol, "A"), item("B", sol, "B")))
	waitFor(t, "give up", func() bool { return a.Stats.Lost.Load() == 1 })
	if _, ok := st.TrendingLastSeen("B"); !ok {
		t.Error("a token whose alert was lost must not be retried on every message")
	}
}
