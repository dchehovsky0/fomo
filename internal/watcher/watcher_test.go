package watcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fomobot/internal/domain"
	"fomobot/internal/fomo"
	"fomobot/internal/launches"
	"fomobot/internal/notify"
	"fomobot/internal/session"
	"fomobot/internal/store"
)

type fakeFomo struct {
	mu     sync.Mutex
	counts map[string]int
	err    error
	calls  atomic.Int64
}

func (f *fakeFomo) set(mint string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts[mint] = n
}

func (f *fakeFomo) SortedThesis(_ context.Context, mint string, after, before time.Time, limit int) (*fomo.TokenThesisPage, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	f.mu.Lock()
	n := f.counts[mint]
	f.mu.Unlock()
	page := &fomo.TokenThesisPage{Count: n}
	for i := range min(n, limit) {
		page.Items = append(page.Items, fomo.Thesis{
			UserID: fmt.Sprint("u", i), Handle: fmt.Sprint("user", i), Ticker: "TKN",
			CreatedAt: after.Add(time.Duration(i+1) * time.Minute), MarketCapAtCreation: float64(1000 * (i + 1)),
		})
	}
	return page, nil
}

type fakeNotifier struct {
	mu      sync.Mutex
	sent    []notify.Alert
	failing atomic.Int64 // number of sends that fail before succeeding
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

func (n *fakeNotifier) signals() []notify.Alert {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]notify.Alert(nil), n.sent...)
}

func testConfig() Config {
	return Config{
		Lifetime: time.Hour, MinTheses: 3, ThesisLimit: 50,
		Schedule:   []Tier{{MaxAge: time.Hour, Every: 20 * time.Millisecond}},
		RetryDelay: 20 * time.Millisecond, AuthPause: 50 * time.Millisecond,
	}
}

func start(t *testing.T, w *Watcher) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
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

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestReleaseDropsATokenWaitingForTheNextCheck(t *testing.T) {
	st, _ := store.Open("")
	w := New(testConfig(), nil, nil, st, quiet())
	w.Add(domain.Token{Mint: "M", Symbol: "S", CreatedAt: time.Now()})
	if !w.Release("M") {
		t.Fatal("release")
	}
	if len(w.Tokens()) != 0 || w.queue.Len() != 0 {
		t.Fatalf("still watched: tokens=%d queue=%d", len(w.Tokens()), w.queue.Len())
	}
	if w.Release("M") {
		t.Fatal("second release")
	}
}

func TestSignalWhenThresholdReached(t *testing.T) {
	f := &fakeFomo{counts: map[string]int{"M": 1}}
	n := &fakeNotifier{}
	st, _ := store.Open("")
	w := New(testConfig(), []Account{{Name: "a", Client: f, Workers: 2}}, n, st, quiet())
	start(t, w)

	w.Add(launches.Launch{Mint: "M", CreatedAt: time.Now().Add(-time.Minute), Dex: "Pump V1", Symbol: "AXM", Name: "Axiom Name"})
	waitFor(t, "a few checks", func() bool { return f.calls.Load() >= 3 })
	if len(n.signals()) != 0 {
		t.Fatal("signal below threshold")
	}
	f.set("M", 3)
	waitFor(t, "signal", func() bool { return len(n.signals()) == 1 })

	s := n.signals()[0]
	if s.Token != "M" || s.Count != 3 || s.Symbol != "TKN" || s.Name != "Axiom Name" || s.Dex != "Pump V1" || s.Kind != notify.KindTheses || s.Threshold != 3 || s.CreatedAt.IsZero() {
		t.Errorf("signal = %+v", s)
	}
	waitFor(t, "stored as signaled and dropped", func() bool { return st.IsSignaled("M") && w.Snapshot().Watching == 0 })
	if delays := w.Delays(); len(delays) != 1 || delays[0].PrevCount != 1 || delays[0].Count != 3 || delays[0].Why == "" {
		t.Fatalf("delay = %+v", delays)
	}
	w.Add(launches.Launch{Mint: "M", CreatedAt: time.Now()})
	if w.Snapshot().Watching != 0 {
		t.Error("signaled token must not be watched again")
	}
	time.Sleep(60 * time.Millisecond)
	if len(n.signals()) != 1 {
		t.Errorf("sent %d signals, want 1", len(n.signals()))
	}
}

// sortedThesis puts the new thesis in items and leaves count behind.
// The alert has to leave on that same response, not on the next poll.
func TestAlertWhenItemsCrossThresholdBeforeCount(t *testing.T) {
	for _, apiCount := range []int{0, 2} {
		f := &itemsAhead{count: apiCount}
		n := &fakeNotifier{}
		st, _ := store.Open("")
		w := New(testConfig(), []Account{{Name: "a", Client: f, Workers: 1}}, n, st, quiet())
		start(t, w)
		w.Add(domain.Token{Mint: "M", Symbol: "S", CreatedAt: time.Now().Add(-time.Minute)})
		waitFor(t, "signal while count lags", func() bool { return len(n.signals()) == 1 })
		if calls := f.calls.Load(); calls != 1 {
			t.Fatalf("api count %d: requests=%d, want the first response", apiCount, calls)
		}
		if got := n.signals()[0].Count; got != 3 {
			t.Fatalf("api count %d: alert count=%d, want 3", apiCount, got)
		}
	}
}

type itemsAhead struct {
	count int
	calls atomic.Int64
}

func (f *itemsAhead) SortedThesis(context.Context, string, time.Time, time.Time, int) (*fomo.TokenThesisPage, error) {
	f.calls.Add(1)
	page := &fomo.TokenThesisPage{Count: f.count}
	for i := range 3 {
		page.Items = append(page.Items, fomo.Thesis{
			UserID: fmt.Sprint("u", i), Handle: fmt.Sprint("user", i), Ticker: "TKN",
			CreatedAt: time.Now().Add(-time.Duration(3-i) * time.Minute),
		})
	}
	return page, nil
}

func TestRejectedAccountDoesNotBlock(t *testing.T) {
	bad := &fakeFomo{err: fmt.Errorf("wrap: %w", fomo.ErrUnauthorized)}
	good := &fakeFomo{counts: map[string]int{}}
	n := &fakeNotifier{}
	st, _ := store.Open("")
	w := New(testConfig(), []Account{{Name: "bad", Client: bad, Workers: 1}, {Name: "good", Client: good, Workers: 1}}, n, st, quiet())
	start(t, w)

	for i := range 5 {
		m := fmt.Sprint("M", i)
		good.set(m, 3)
		w.Add(launches.Launch{Mint: m, CreatedAt: time.Now()})
	}
	waitFor(t, "all signals via the good account", func() bool { return len(n.signals()) == 5 })
	if w.Stats.AuthErrors.Load() == 0 {
		t.Error("expected auth errors from the bad account")
	}
}

func TestBrokenAccountsHandOverImmediately(t *testing.T) {
	noLogin := &fakeFomo{err: fmt.Errorf("get access token: %w", session.ErrNoToken)}
	deadProxy := &fakeFomo{err: errors.New("proxyconnect tcp: connection refused")}
	good := &fakeFomo{counts: map[string]int{}}
	n := &fakeNotifier{}
	st, _ := store.Open("")
	cfg := testConfig()
	cfg.RetryDelay, cfg.AuthPause = 10*time.Second, 10*time.Second // a delayed token would miss the deadline
	w := New(cfg, []Account{
		{Name: "no-login", Client: noLogin, Workers: 1},
		{Name: "dead-proxy", Client: deadProxy, Workers: 1},
		{Name: "good", Client: good, Workers: 1},
	}, n, st, quiet())
	start(t, w)

	for i := range 5 {
		m := fmt.Sprint("M", i)
		good.set(m, 3)
		w.Add(launches.Launch{Mint: m, CreatedAt: time.Now()})
	}
	waitFor(t, "all signals via the good account without delay", func() bool { return len(n.signals()) == 5 })
	if noLogin.calls.Load() > 1 || deadProxy.calls.Load() > 1 {
		t.Errorf("broken accounts must rest after a failure: no-login=%d dead-proxy=%d", noLogin.calls.Load(), deadProxy.calls.Load())
	}
}

func TestSendFailureIsRetried(t *testing.T) {
	f := &fakeFomo{counts: map[string]int{"M": 3}}
	n := &fakeNotifier{}
	n.failing.Store(2)
	st, _ := store.Open("")
	w := New(testConfig(), []Account{{Name: "a", Client: f, Workers: 1}}, n, st, quiet())
	start(t, w)

	w.Add(launches.Launch{Mint: "M", CreatedAt: time.Now()})
	waitFor(t, "signal after retries", func() bool { return st.IsSignaled("M") })
	if w.Stats.SendErrors.Load() != 2 || len(n.signals()) != 1 {
		t.Errorf("send errors = %d, signaled = %v", w.Stats.SendErrors.Load(), st.IsSignaled("M"))
	}
}

func TestTokenExpires(t *testing.T) {
	f := &fakeFomo{counts: map[string]int{"M": 1}}
	st, _ := store.Open("")
	cfg := testConfig()
	cfg.Lifetime = 150 * time.Millisecond
	w := New(cfg, []Account{{Name: "a", Client: f, Workers: 1}}, &fakeNotifier{}, st, quiet())
	start(t, w)

	w.Add(launches.Launch{Mint: "M", CreatedAt: time.Now()})
	// Creation age does not block entry. The lifetime runs from this moment.
	w.Add(launches.Launch{Mint: "OLD", CreatedAt: time.Now().Add(-time.Hour)})
	if got := w.Snapshot().Watching; got != 2 {
		t.Fatalf("watching = %d, both tokens must enter", got)
	}
	waitFor(t, "expiry", func() bool { return w.Snapshot().Watching == 0 })
	if w.Stats.Expired.Load() != 2 || w.Stats.Checks.Load() < 3 {
		t.Errorf("expired = %d, checks = %d", w.Stats.Expired.Load(), w.Stats.Checks.Load())
	}
}

// blockUntilStop stays inside the fomo call until the bot is stopped, then
// fails with the cancelled context. That is what a real request does on shutdown.
type blockUntilStop struct{ calls atomic.Int64 }

func (f *blockUntilStop) SortedThesis(ctx context.Context, _ string, _, _ time.Time, _ int) (*fomo.TokenThesisPage, error) {
	f.calls.Add(1)
	<-ctx.Done()
	return nil, ctx.Err()
}

// Stopping the bot used to put every due token back on the queue and pop it
// again immediately, so each worker logged "fomo request" in a tight loop.
func TestStopDoesNotRespinDueTokens(t *testing.T) {
	f := &blockUntilStop{}
	st, _ := store.Open("")
	accounts := make([]Account, 8)
	for i := range accounts {
		accounts[i] = Account{Name: fmt.Sprint("a", i), Client: f, Workers: 2}
	}
	w := New(testConfig(), accounts, &fakeNotifier{}, st, quiet())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	now := time.Now()
	for i := range 40 {
		w.Add(launches.Launch{Mint: fmt.Sprint("M", i), CreatedAt: now, Symbol: "T"})
	}
	waitFor(t, "checks started", func() bool { return f.calls.Load() > 0 })
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher kept running after stop")
	}
	after := f.calls.Load()
	time.Sleep(50 * time.Millisecond)
	if extra := f.calls.Load() - after; extra > 0 {
		t.Fatalf("checks continued after stop: %d", extra)
	}
}

type jumpClock struct {
	fakeFomo
	advance func()
}

func (j *jumpClock) SortedThesis(ctx context.Context, mint string, after, before time.Time, limit int) (*fomo.TokenThesisPage, error) {
	if j.advance != nil {
		j.advance()
	}
	return j.fakeFomo.SortedThesis(ctx, mint, after, before, limit)
}

func TestLateCheckWaitsFullInterval(t *testing.T) {
	st, _ := store.Open("")
	base := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	var now atomic.Int64
	now.Store(base.UnixNano())
	f := &jumpClock{fakeFomo: fakeFomo{counts: map[string]int{"M": 0}}, advance: func() {
		now.Store(base.Add(5 * time.Second).UnixNano())
	}}
	cfg := testConfig()
	cfg.Schedule = []Tier{{MaxAge: time.Hour, Every: 2 * time.Second}}
	w := New(cfg, []Account{{Name: "a", Client: f}}, &fakeNotifier{}, st, quiet())
	w.now = func() time.Time { return time.Unix(0, now.Load()).UTC() }
	it := &item{l: domain.Token{Mint: "M", Symbol: "S", CreatedAt: base}, due: base, addedAt: base, idx: -1}
	w.items[it.l.Mint] = it
	if _, err := w.check(context.Background(), 0, it); err != nil {
		t.Fatal(err)
	}
	want := base.Add(7 * time.Second)
	if !it.due.Equal(want) {
		t.Fatalf("due = %s, want %s", it.due, want)
	}
}

func TestFastCheckStaysOnTheTierInterval(t *testing.T) {
	st, _ := store.Open("")
	base := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	f := &fakeFomo{counts: map[string]int{"M": 1}}
	cfg := testConfig()
	cfg.Schedule = []Tier{{MaxAge: time.Hour, Every: 2 * time.Second}}
	w := New(cfg, []Account{{Name: "a", Client: f}}, &fakeNotifier{}, st, quiet())
	w.now = func() time.Time { return base }
	it := &item{l: domain.Token{Mint: "M", Symbol: "S", CreatedAt: base}, due: base, addedAt: base, idx: -1}
	w.items[it.l.Mint] = it
	if _, err := w.check(context.Background(), 0, it); err != nil {
		t.Fatal(err)
	}
	if !it.due.Equal(base.Add(2 * time.Second)) {
		t.Fatalf("due = %s, want the 2s tier and no later", it.due)
	}

	f.err = errors.New("proxyconnect tcp: connection refused")
	it.due = base
	it.idx = -1
	if _, err := w.check(context.Background(), 0, it); err == nil {
		t.Fatal("expected retryable error")
	}
	if !it.due.Equal(base.Add(2 * time.Second)) {
		t.Fatalf("retry due = %s, want the tier interval, not an immediate extra request", it.due)
	}
}

func TestWaveIsNotDueTogether(t *testing.T) {
	st, _ := store.Open("")
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	cfg := testConfig()
	cfg.Schedule = []Tier{{MaxAge: time.Hour, Every: 2 * time.Second}}
	w := New(cfg, nil, nil, st, quiet())
	w.now = func() time.Time { return now }
	for _, mint := range []string{"a", "b", "c"} {
		w.Add(domain.Token{Mint: mint, CreatedAt: now})
	}
	var dues []time.Time
	for _, mint := range []string{"a", "b", "c"} {
		dues = append(dues, w.items[mint].due)
	}
	if !dues[0].Equal(now) || !dues[1].Equal(now.Add(200*time.Millisecond)) || !dues[2].Equal(now.Add(400*time.Millisecond)) {
		t.Fatalf("dues = %v, a tier wave must not be due at one moment", dues)
	}
}

func TestVolumeTokensLeaveAfterTier3(t *testing.T) {
	st, _ := store.Open("")
	cfg := testConfig()
	cfg.VolumeFor = time.Hour
	cfg.Lifetime = 10 * time.Hour
	cfg.Schedule = []Tier{{MaxAge: time.Minute, Every: 20 * time.Millisecond}, {MaxAge: 10 * time.Hour, Every: time.Minute}}
	w := New(cfg, nil, nil, st, quiet())
	now := time.Now()
	w.now = func() time.Time { return now }
	w.Add(domain.Token{Mint: "young", CreatedAt: now.Add(-5 * time.Hour)})
	w.Add(domain.Token{Mint: "old", CreatedAt: now})
	w.mu.Lock()
	w.items["old"].addedAt = now.Add(-time.Hour)
	w.mu.Unlock()
	var got []string
	for _, tok := range w.VolumeTokens() {
		got = append(got, tok.Mint)
	}
	if len(got) != 1 || got[0] != "young" {
		t.Fatalf("volume tokens = %v", got)
	}
	if w.interval(w.inWork(w.items["young"], now)) != 20*time.Millisecond {
		t.Fatal("poll interval must follow time in work, not token age")
	}
}

type memTiers struct {
	mu      sync.Mutex
	tier    int
	entered time.Time
	steps   []int
	status  string
}

func (b *memTiers) EnterTier(_ context.Context, _ domain.Token, at time.Time) (int, time.Time, string, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tier == 0 {
		b.tier = 1
		b.entered = at
		b.steps = append(b.steps, 1)
		b.status = "watching"
		return 1, at, "watching", true, nil
	}
	return b.tier, b.entered, b.status, false, nil
}

func (b *memTiers) AdvanceTier(_ context.Context, _ string, tier int, _ time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tier = tier
	b.steps = append(b.steps, tier)
	return nil
}

func (b *memTiers) SetWatchStatus(_ context.Context, _, status string, _ time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.status = status
	return nil
}

func (b *memTiers) snapshot() (int, []int, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tier, append([]int(nil), b.steps...), b.status
}

func TestTierHistoryFollowsTheSchedule(t *testing.T) {
	st, _ := store.Open("")
	book := &memTiers{}
	w := New(Config{
		Lifetime: 3 * time.Hour,
		Schedule: []Tier{
			{MaxAge: 20 * time.Minute, Every: time.Second},
			{MaxAge: 50 * time.Minute, Every: time.Second},
			{MaxAge: 110 * time.Minute, Every: time.Second},
			{MaxAge: 3 * time.Hour, Every: time.Second},
		},
	}, nil, nil, st, quiet())
	w.SetTiers(book)
	now := time.Now()
	w.now = func() time.Time { return now }
	w.Add(domain.Token{Mint: "M", Symbol: "S", CreatedAt: now})
	if tier, _, _ := book.snapshot(); tier != 1 {
		t.Fatalf("tier after enter = %d", tier)
	}

	w.mu.Lock()
	it := w.items["M"]
	w.mu.Unlock()
	it.addedAt = now.Add(-20 * time.Minute)
	w.noteTier(it, now)
	it.addedAt = now.Add(-50 * time.Minute)
	w.noteTier(it, now)
	it.addedAt = now.Add(-110 * time.Minute)
	w.noteTier(it, now)
	w.noteTier(it, now)
	tier, steps, status := book.snapshot()
	if tier != 4 || status != "watching" || strings.Join(ints(steps), ",") != "1,2,3,4" {
		t.Fatalf("tier=%d status=%s steps=%v", tier, status, steps)
	}
	if !w.Release("M") {
		t.Fatal("release")
	}
	if _, _, status = book.snapshot(); status != "dropped" {
		t.Fatalf("status after drop = %s", status)
	}
	if _, steps, _ = book.snapshot(); strings.Join(ints(steps), ",") != "1,2,3,4" {
		t.Fatalf("drop must keep tier history %v", steps)
	}

	young := &memTiers{}
	w.SetTiers(young)
	w.now = func() time.Time { return now }
	w.Add(domain.Token{Mint: "FLAT", CreatedAt: now})
	w.mu.Lock()
	w.items["FLAT"].addedAt = now.Add(-25 * time.Minute)
	w.mu.Unlock()
	if !w.Release("FLAT") {
		t.Fatal("release flat")
	}
	tier, steps, status = young.snapshot()
	if tier != 1 || status != "dropped" || strings.Join(ints(steps), ",") != "1" {
		t.Fatalf("dropped token advanced: tier=%d status=%s steps=%v", tier, status, steps)
	}

	again := &memTiers{tier: 4, status: "dropped", steps: []int{1, 2, 3, 4}, entered: now.Add(-2 * time.Hour)}
	w.SetTiers(again)
	w.Add(domain.Token{Mint: "DONE", Symbol: "D", CreatedAt: now})
	if _, ok := w.items["DONE"]; ok {
		t.Fatal("a finished token must not be watched again")
	}
	if _, steps, status = again.snapshot(); status != "dropped" || strings.Join(ints(steps), ",") != "1,2,3,4" {
		t.Fatalf("finished row changed: status=%s steps=%v", status, steps)
	}
}

func ints(v []int) []string {
	out := make([]string, len(v))
	for i, n := range v {
		out[i] = fmt.Sprint(n)
	}
	return out
}

func TestIntervalTiers(t *testing.T) {
	cfg := testConfig()
	cfg.Schedule = []Tier{{MaxAge: time.Hour, Every: time.Minute}, {MaxAge: 10 * time.Minute, Every: 30 * time.Second}, {MaxAge: 6 * time.Hour, Every: 3 * time.Minute}}
	w := New(cfg, nil, nil, nil, quiet())
	cases := map[time.Duration]time.Duration{
		time.Minute: 30 * time.Second, 30 * time.Minute: time.Minute, 2 * time.Hour: 3 * time.Minute, 7 * time.Hour: 3 * time.Minute,
	}
	for age, want := range cases {
		if got := w.interval(age); got != want {
			t.Errorf("interval(%v) = %v, want %v", age, got, want)
		}
	}
}

func TestGapPartsNameTheMeasuredCause(t *testing.T) {
	at := func(m, s int, extra time.Duration) time.Time {
		return time.Date(2026, 10, 1, 17, m, s, 0, time.Local).Add(extra)
	}
	ms := time.Millisecond
	sum := func(d Delay) int64 {
		return d.NotWatchedMS + d.BlindMS + d.SlotMS + d.FailMS + d.LateMS + d.FomoMS + d.AssembleMS + d.SendMS + d.OtherMS
	}
	cases := []struct {
		name   string
		d      Delay
		want   map[string]int64
		say    string
		notSay []string
	}{
		{
			name: "fomo still answered below the threshold",
			d: Delay{Threshold: 3, Count: 5, PrevCount: 2, EveryMS: 5000,
				ThesisAt: at(6, 42, 0), PrevAt: at(7, 30, 300*ms), ScheduledAt: at(7, 35, 0), DueAt: at(7, 35, 0),
				StartedAt: at(7, 35, 0), SeenAt: at(7, 35, 300*ms), SentAt: at(7, 35, 650*ms),
				FomoMS: 300, AssembleMS: 200, SendMS: 150},
			want:   map[string]int64{"blind": 48300, "slot": 4700, "late": 0, "fail": 0},
			say:    "ещё отдал 2",
			notSay: []string{"очередь:", "неудачные"},
		},
		{
			name: "the poll started late",
			d: Delay{Threshold: 3, Count: 5, PrevCount: 2, EveryMS: 5000,
				ThesisAt: at(6, 42, 0), PrevAt: at(6, 40, 0), ScheduledAt: at(6, 45, 0), DueAt: at(6, 45, 0),
				StartedAt: at(7, 35, 0), SeenAt: at(7, 35, 300*ms), SentAt: at(7, 35, 300*ms),
				FomoMS: 300},
			want:   map[string]int64{"blind": 0, "slot": 3000, "late": 50000, "fail": 0},
			say:    "очередь:",
			notSay: []string{"ещё отдал", "неудачные"},
		},
		{
			name: "a failed request paused the token",
			d: Delay{Threshold: 3, Count: 5, PrevCount: 2, EveryMS: 5000, Fails: 1, FailKind: "error",
				ThesisAt: at(6, 42, 0), PrevAt: at(6, 40, 0), ScheduledAt: at(6, 45, 0), DueAt: at(7, 15, 0),
				StartedAt: at(7, 15, 0), SeenAt: at(7, 15, 200*ms), SentAt: at(7, 15, 200*ms),
				FomoMS: 200, failSpans: []span{{at(6, 45, 0), at(7, 15, 0)}}},
			want:   map[string]int64{"slot": 3000, "fail": 30000, "late": 0},
			say:    "неудачные попытки (1",
			notSay: []string{"очередь:", "ещё отдал"},
		},
		{
			name: "the thesis came before the token was watched",
			d: Delay{Threshold: 3, Count: 3,
				ThesisAt: at(6, 42, 0), AddedAt: at(7, 0, 0), DueAt: at(7, 0, 0),
				StartedAt: at(7, 0, 0), SeenAt: at(7, 0, 300*ms), SentAt: at(7, 0, 300*ms),
				FomoMS: 300},
			want:   map[string]int64{"not_watched": 18000, "late": 0, "blind": 0},
			say:    "до того, как токен попал в работу",
			notSay: []string{"очередь:", "ещё отдал"},
		},
		{
			name: "the thesis was marked during the request",
			d: Delay{Threshold: 3, Count: 3, PrevAt: at(6, 40, 0), PrevCount: 2,
				ThesisAt: at(6, 45, 100*ms), ScheduledAt: at(6, 45, 0), DueAt: at(6, 45, 0),
				StartedAt: at(6, 45, 0), SeenAt: at(6, 45, 400*ms), SentAt: at(6, 45, 400*ms),
				FomoMS: 400},
			want: map[string]int64{"fomo": 300, "slot": 0, "late": 0},
			say:  "задержек дольше секунды нет",
		},
	}
	for _, c := range cases {
		d := c.d
		fillParts(&d)
		got := map[string]int64{"not_watched": d.NotWatchedMS, "blind": d.BlindMS, "slot": d.SlotMS,
			"fail": d.FailMS, "late": d.LateMS, "fomo": d.FomoMS}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%s: %s = %d, want %d (%+v)", c.name, k, got[k], v, got)
			}
		}
		if sum(d) != d.GapMS {
			t.Errorf("%s: parts %d != gap %d", c.name, sum(d), d.GapMS)
		}
		if d.OtherMS < 0 || d.OtherMS >= 1000 {
			t.Errorf("%s: other = %d", c.name, d.OtherMS)
		}
		why := explain(d)
		if !strings.Contains(why, c.say) {
			t.Errorf("%s: want %q in %s", c.name, c.say, why)
		}
		for _, bad := range c.notSay {
			if strings.Contains(why, bad) {
				t.Errorf("%s: %q must not be named: %s", c.name, bad, why)
			}
		}
	}

	partial := &fomo.TokenThesisPage{Count: 5, HasNextPage: true, Items: []fomo.Thesis{
		{CreatedAt: at(6, 42, 0)}, {CreatedAt: at(6, 40, 0)}, {CreatedAt: at(6, 3, 0)},
	}}
	if _, ok := thresholdThesisAt(partial, 3); ok {
		t.Fatal("a partial page must not give the 3rd thesis time")
	}
	full := &fomo.TokenThesisPage{Count: 3, Items: []fomo.Thesis{
		{CreatedAt: at(6, 42, 0)}, {CreatedAt: at(6, 40, 0)}, {CreatedAt: at(6, 3, 0)},
	}}
	if got, ok := thresholdThesisAt(full, 3); !ok || !got.Equal(at(6, 42, 0)) {
		t.Fatalf("3rd thesis = %v %v", got, ok)
	}
}

func TestFailedSendIsRecordedInTheGap(t *testing.T) {
	f := &fakeFomo{counts: map[string]int{"M": 3}}
	n := &fakeNotifier{}
	n.failing.Store(1)
	st, _ := store.Open("")
	w := New(testConfig(), []Account{{Name: "a", Client: f, Workers: 1}}, n, st, quiet())
	start(t, w)
	w.Add(launches.Launch{Mint: "M", CreatedAt: time.Now().Add(-time.Minute)})
	waitFor(t, "alert after a failed send", func() bool { return len(w.Delays()) == 1 })
	d := w.Delays()[0]
	if d.Fails != 1 || d.FailKind != "send" || len(n.signals()) != 1 {
		t.Fatalf("delay = %+v", d)
	}
}
