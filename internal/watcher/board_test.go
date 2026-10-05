package watcher

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"fomobot/internal/domain"
	"fomobot/internal/fomo"
	"fomobot/internal/store"
)

func prodSchedule() []Tier {
	return []Tier{
		{MaxAge: 20 * time.Minute, Every: 2 * time.Second},
		{MaxAge: 50 * time.Minute, Every: 5 * time.Second},
		{MaxAge: 110 * time.Minute, Every: 30 * time.Second},
		{MaxAge: 49*time.Hour + 50*time.Minute, Every: 2 * time.Minute},
	}
}

func TestBoardGroupsByTheSchedule(t *testing.T) {
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.MinTheses = 3
	cfg.Schedule = prodSchedule()
	cfg.Lifetime = 49*time.Hour + 50*time.Minute
	w := New(cfg, nil, nil, st, quiet())
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return now }

	empty := w.Board()
	if empty.Watching != 0 || len(empty.Tokens) != 0 || len(empty.Tiers) != 4 {
		t.Fatalf("empty board: %+v", empty)
	}
	if got := time.Duration(empty.Tiers[3].LastsMS) * time.Millisecond; got != 48*time.Hour {
		t.Fatalf("tier 4 lasts %s, want 48h", got)
	}

	put := func(mint string, age, due time.Duration) *item {
		w.Add(domain.Token{Mint: mint, Symbol: mint, Name: mint + " name", Dex: "Pump V1", Pool: mint + "-pool", CreatedAt: now.Add(-age)})
		w.mu.Lock()
		it := w.items[mint]
		it.addedAt = now.Add(-age)
		it.due = now.Add(due)
		w.mu.Unlock()
		return it
	}
	fresh := put("fresh", 4*time.Minute, 1500*time.Millisecond)
	put("mid", 25*time.Minute, -5*time.Second)
	older := put("older", 60*time.Minute, 20*time.Second)
	put("late", 3*time.Hour, time.Minute)
	w.publishCount(fresh, 2)
	w.miss(older, "error", now.Add(-time.Second), now)

	b := w.Board()
	if b.Need != 3 || b.Watching != 4 {
		t.Fatalf("need=%d watching=%d", b.Need, b.Watching)
	}
	wantN := []int{1, 1, 1, 1}
	for i, tier := range b.Tiers {
		if tier.Tokens != wantN[i] {
			t.Fatalf("tier %d tokens = %d", tier.Tier, tier.Tokens)
		}
	}
	if len(b.Tokens) != 4 || b.Tokens[0].Mint != "fresh" || b.Tokens[3].Mint != "late" {
		t.Fatalf("order: %+v", b.Tokens)
	}
	f := b.Tokens[0]
	if f.Tier != 1 || f.AgeMS != (4*time.Minute).Milliseconds() || f.LeftMS != (16*time.Minute).Milliseconds() || f.EveryMS != 2000 {
		t.Fatalf("fresh: %+v", f)
	}
	if !f.Checked || f.Theses != 2 || f.Checks != 1 || f.NextMS != 1500 {
		t.Fatalf("fresh count: %+v", f)
	}
	if b.Tokens[1].Tier != 2 || b.Tokens[1].Checked || b.Tokens[1].NextMS != -5000 {
		t.Fatalf("mid: %+v", b.Tokens[1])
	}
	if b.Tokens[2].Tier != 3 || b.Tokens[2].FailKind != "error" || b.Tokens[2].LeftMS != (50*time.Minute).Milliseconds() {
		t.Fatalf("older: %+v", b.Tokens[2])
	}
	if b.Tokens[3].Tier != 4 || b.Tokens[3].EveryMS != (2*time.Minute).Milliseconds() {
		t.Fatalf("late: %+v", b.Tokens[3])
	}

	w.clearMiss(older)
	if got := w.Board().Tokens[2].FailKind; got != "" {
		t.Fatalf("fail cleared, still %q", got)
	}

	rec := httptest.NewRecorder()
	w.BoardHandler("https://fomo.family/tokens/solana/{mint}", "https://axiom.trade/meme/{pair}?chain=sol").
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/board", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var got Board
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Tokens[0].FomoURL != "https://fomo.family/tokens/solana/fresh" {
		t.Fatalf("fomo url %s", got.Tokens[0].FomoURL)
	}
	if got.Tokens[0].AxiomURL != "https://axiom.trade/meme/fresh-pool?chain=sol" {
		t.Fatalf("axiom url %s", got.Tokens[0].AxiomURL)
	}
}

func TestLiveTokenShowsLastPoll(t *testing.T) {
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	w := New(testConfig(), nil, nil, st, quiet())
	w.cfg.MinTheses = 3
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return now }
	w.Add(domain.Token{
		Mint: "fresh", Symbol: "MEW", Name: "cat", Dex: "pumpfun", Pool: "fresh-pool",
		CreatedAt: now.Add(-4 * time.Minute),
	})
	w.mu.Lock()
	it := w.items["fresh"]
	it.addedAt = now.Add(-4 * time.Minute)
	w.mu.Unlock()

	if _, ok := w.Live("fresh", "", ""); !ok {
		t.Fatal("missing fresh")
	}
	blank, ok := w.Live("fresh", "https://fomo.family/tokens/solana/{mint}", "https://axiom.trade/meme/{pair}?chain=sol")
	if !ok || blank.Checked || len(blank.Theses) != 0 || blank.FomoURL == "" || blank.AxiomURL == "" {
		t.Fatalf("before poll: %+v", blank)
	}

	w.publishPage(it, &fomo.TokenThesisPage{
		Count: 1,
		Items: []fomo.Thesis{
			{Handle: "early", Comment: "ранний", CreatedAt: now.Add(-time.Minute), PositionUSD: 800},
			{Handle: "late", Comment: "  свежий   текст ", CreatedAt: now, PositionUSD: 128000, PositionClosed: true, TokenImageURL: "https://img.example/t.png"},
		},
	}, now)

	d, ok := w.Live("fresh", "https://fomo.family/tokens/solana/{mint}", "https://axiom.trade/meme/{pair}?chain=sol")
	if !ok || !d.Live || !d.Checked || d.Count != 2 || d.Need != 3 || d.Tier != 1 || d.Symbol != "MEW" {
		t.Fatalf("live: %+v", d)
	}
	if d.ImageURL != "https://img.example/t.png" || !d.CheckedAt.Equal(now) {
		t.Fatalf("meta: %+v", d)
	}
	if len(d.Theses) != 2 || d.Theses[1].Text != "свежий текст" || !d.Theses[1].Closed || d.Theses[0].PositionUSD != 800 {
		t.Fatalf("theses: %+v", d.Theses)
	}
	if d.FomoURL != "https://fomo.family/tokens/solana/fresh" || d.AxiomURL != "https://axiom.trade/meme/fresh-pool?chain=sol" {
		t.Fatalf("links %s %s", d.FomoURL, d.AxiomURL)
	}
	w.mu.Lock()
	it.l.Deployer = "DEP111111111111111111111111111111111111111"
	it.l.EntryMarketCap = 9000
	it.l.EntryVolumeUSD = 500
	it.l.LiquiditySOL = 12
	it.l.Twitter = "https://x.com/mew"
	w.mu.Unlock()
	w.NoteQuote("fresh", 12000, 4000, 1500, true, "", now)
	d, ok = w.Live("fresh", "", "")
	if !ok || d.MarketCap != 12000 || d.VolumeUSD != 4000 || d.LiquidityUSD != 1500 || d.EntryMarketCap != 9000 || d.EntryVolumeUSD != 500 || d.LiquiditySOL != 12 || d.Deployer == "" || d.Twitter == "" {
		t.Fatalf("quote: %+v", d)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/watch/fresh", nil)
	req.SetPathValue("mint", "fresh")
	w.LiveHandler("https://fomo.family/tokens/solana/{mint}", "").ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "свежий текст") {
		t.Fatalf("handler %d %s", rec.Code, rec.Body.String())
	}
	miss := httptest.NewRecorder()
	missReq := httptest.NewRequest(http.MethodGet, "/api/watch/gone", nil)
	missReq.SetPathValue("mint", "gone")
	w.LiveHandler("", "").ServeHTTP(miss, missReq)
	if miss.Code != http.StatusNotFound {
		t.Fatalf("missing status %d", miss.Code)
	}
}

func TestBoardPublishDoesNotRace(t *testing.T) {
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	w := New(testConfig(), nil, nil, st, quiet())
	w.Add(domain.Token{Mint: "M", Symbol: "M", CreatedAt: time.Now()})
	it := w.items["M"]
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				w.publishCount(it, 1)
				w.miss(it, "retry", time.Now(), time.Now())
				w.clearMiss(it)
				_ = w.Board()
			}
		}
	}()
	time.Sleep(30 * time.Millisecond)
	close(stop)
	wg.Wait()
}
