package screen

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"fomobot/internal/domain"
)

type fakeQ struct {
	mu     sync.Mutex
	limit  int
	vols   map[string]domain.Volume
	failed []string
	calls  int
	seq    [][]string
	pools  []string
	err    error
}

func (f *fakeQ) Limit() int { return f.limit }
func (f *fakeQ) Wave(_ context.Context, tokens []domain.Token) (map[string]domain.Volume, []string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	mints := make([]string, len(tokens))
	for i, tok := range tokens {
		mints[i] = tok.Mint
		f.pools = append(f.pools, tok.Pool)
	}
	f.seq = append(f.seq, mints)
	return f.vols, f.failed, f.err
}

func vol5m(usd float64) domain.Volume {
	return domain.Volume{USD5m: usd, USD1h: usd, Trades5m: 1, Trades1h: 1}
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestGateFiltersByVolume(t *testing.T) {
	q := &fakeQ{limit: 300, vols: map[string]domain.Volume{
		"live": vol5m(1500),
		"dead": vol5m(400),
		"edge": vol5m(1000),
	}}
	g := New(Config{After: 10 * time.Second, MinVolume5m: 1000, Retry: time.Second}, q, quiet())
	now := time.Now()
	g.now = func() time.Time { return now }
	g.Add(domain.Token{Mint: "live", Symbol: "L", CreatedAt: now.Add(-time.Minute)})
	g.Add(domain.Token{Mint: "dead", Symbol: "D", CreatedAt: now.Add(-time.Minute)})
	g.Add(domain.Token{Mint: "edge", Symbol: "E", CreatedAt: now.Add(-time.Minute)})
	g.Add(domain.Token{Mint: "new", Symbol: "N", CreatedAt: now})

	var got []string
	var passed domain.Token
	g.wave(context.Background(), func(token domain.Token) {
		got = append(got, token.Mint)
		passed = token
	})
	if len(got) != 1 || got[0] != "live" || passed.EntryVolume != vol5m(1500) {
		t.Fatalf("emitted %v volume %+v", got, passed.EntryVolume)
	}
	g.mu.Lock()
	_, dead := g.pending["dead"]
	_, edge := g.pending["edge"]
	_, young := g.pending["new"]
	g.mu.Unlock()
	if dead || edge {
		t.Fatalf("low volume stayed pending: dead=%v edge=%v", dead, edge)
	}
	if !young {
		t.Fatal("young token was checked too soon")
	}

	q.vols = map[string]domain.Volume{}
	now = now.Add(time.Second)
	g.wave(context.Background(), func(domain.Token) { t.Fatal("no volume yet must be retried, not emitted") })
	g.mu.Lock()
	_, live := g.pending["live"]
	_, still := g.pending["new"]
	g.mu.Unlock()
	if live {
		t.Fatal("live token was put back")
	}
	if !still {
		t.Fatal("token with no volume was dropped")
	}
}

func TestMissingVolumeIsRetried(t *testing.T) {
	q := &fakeQ{limit: 10, vols: map[string]domain.Volume{
		"cheap": vol5m(40),
	}}
	g := New(Config{After: 10 * time.Second, MinVolume5m: 1000, Retry: time.Second}, q, quiet())
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
	g.wave(context.Background(), func(domain.Token) { t.Fatal("token missing from axiom must stay pending") })
	g.mu.Lock()
	_, still := g.pending["gone"]
	g.mu.Unlock()
	if !still {
		t.Fatal("missing token was dropped")
	}
}

func TestEmptyVolumeRetriesOnTheOrdinaryInterval(t *testing.T) {
	q := &fakeQ{limit: 10, vols: map[string]domain.Volume{"late": {}}}
	g := New(Config{After: time.Second, MinVolume5m: 1000, Retry: 30 * time.Second}, q, quiet())
	now := time.Now()
	g.now = func() time.Time { return now }
	g.Add(domain.Token{Mint: "late", Pool: "PAIR", CreatedAt: now.Add(-time.Minute)})
	g.wave(context.Background(), func(domain.Token) { t.Fatal("empty volume must stay pending") })
	g.mu.Lock()
	due := g.pending["late"].due
	g.mu.Unlock()
	if !due.Equal(now.Add(30 * time.Second)) {
		t.Fatalf("next ask in %s, want 30s", due.Sub(now))
	}
}

func TestEmptyVolumeDropsAfterNoVolumeFor(t *testing.T) {
	q := &fakeQ{limit: 10, vols: map[string]domain.Volume{}, failed: []string{"silent", "nopool"}}
	g := New(Config{After: 10 * time.Second, MinVolume5m: 1000, Retry: 30 * time.Second, NoVolumeFor: 2 * time.Minute}, q, quiet())
	now := time.Now()
	g.now = func() time.Time { return now }
	g.Add(domain.Token{Mint: "young", Pool: "P1", CreatedAt: now.Add(-119 * time.Second)})
	g.Add(domain.Token{Mint: "dead", Pool: "P2", CreatedAt: now.Add(-2 * time.Minute)})
	g.Add(domain.Token{Mint: "silent", Pool: "P3", CreatedAt: now.Add(-10 * time.Minute)})
	g.Add(domain.Token{Mint: "nopool", CreatedAt: now.Add(-3 * time.Minute)})
	g.wave(context.Background(), func(domain.Token) { t.Fatal("nothing has volume") })
	g.mu.Lock()
	_, young := g.pending["young"]
	_, dead := g.pending["dead"]
	_, silent := g.pending["silent"]
	_, nopool := g.pending["nopool"]
	g.mu.Unlock()
	if !young || dead || !silent || nopool {
		t.Fatalf("young=%v dead=%v silent=%v nopool=%v", young, dead, silent, nopool)
	}
	h := g.Health()
	if h.Dropped != 2 || h.NeverVolume != 2 {
		t.Fatalf("health = %+v", h)
	}
}

func TestVolumeHealthSaysWhichRequestReturnedVolume(t *testing.T) {
	q := &fakeQ{limit: 10, vols: map[string]domain.Volume{}}
	g := New(Config{After: 10 * time.Second, MinVolume5m: 1000, Retry: time.Second}, q, quiet())
	now := time.Now()
	g.now = func() time.Time { return now }
	g.Add(domain.Token{Mint: "late", Symbol: "L", CreatedAt: now.Add(-time.Minute)})
	g.Add(domain.Token{Mint: "now", Symbol: "N", CreatedAt: now.Add(-time.Minute)})
	g.wave(context.Background(), func(domain.Token) { t.Fatal("nothing traded yet") })
	q.vols = map[string]domain.Volume{
		"late": {USD5m: 2400, USD1h: 2400, Trades5m: 31, Trades1h: 31},
	}
	now = now.Add(time.Second)
	var got []string
	g.wave(context.Background(), func(token domain.Token) { got = append(got, token.Mint) })
	if len(got) != 1 || got[0] != "late" {
		t.Fatalf("emitted %v", got)
	}
	h := g.Health()
	if h.Tokens != 2 || h.Requests != 4 || h.VolumeFromRequest[2] != 1 || h.Passed != 1 || h.Dropped != 0 || h.Waiting != 1 {
		t.Fatalf("health = %+v", h)
	}
	byMint := map[string]VolToken{}
	for _, tok := range h.Recent {
		byMint[tok.Mint] = tok
	}
	nowTok, lateTok := byMint["now"], byMint["late"]
	if nowTok.Decision != "" || len(nowTok.Attempts) != 2 || nowTok.Attempts[0].N != 1 || nowTok.Attempts[1].Volume5m != 0 {
		t.Fatalf("now = %+v", nowTok)
	}
	if lateTok.Decision != "в работу" || lateTok.Attempts[1].Volume5m != 2400 || lateTok.Attempts[1].Trades5m != 31 || lateTok.Attempts[1].N != 2 {
		t.Fatalf("late = %+v", lateTok)
	}
}

type memBook struct {
	tokens []domain.Token
	noted  []domain.Volume
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
func (b *memBook) NoteVolume(_ string, v domain.Volume, _ time.Time) {
	b.noted = append(b.noted, v)
}

func TestRecheckSkipsATokenThatLeftTheBook(t *testing.T) {
	q := &fakeQ{vols: map[string]domain.Volume{"gone": vol5m(9000), "stay": vol5m(9000)}}
	g := New(Config{Recheck: 30 * time.Second, MinVolume5m: 1000}, q, quiet())
	now := time.Now()
	g.now = func() time.Time { return now }
	book := &memBook{tokens: []domain.Token{{Mint: "stay"}}}
	g.vol["gone"] = &volTrack{next: now.Add(-time.Second)}
	g.recheckOne(context.Background(), book, domain.Token{Mint: "gone"})
	if q.calls != 0 {
		t.Fatalf("released token was asked: %d", q.calls)
	}
	g.mu.Lock()
	_, still := g.vol["gone"]
	g.mu.Unlock()
	if still {
		t.Fatal("volume track kept for a token that left")
	}
	g.recheckOne(context.Background(), book, domain.Token{Mint: "stay"})
	if q.calls != 1 {
		t.Fatalf("watched token was not asked: %d", q.calls)
	}
}

func TestVolumeHistoryStaysBounded(t *testing.T) {
	g := New(Config{MinVolume5m: 1000}, &fakeQ{limit: 10}, quiet())
	it := &item{token: domain.Token{Mint: "spin"}}
	for n := 1; n <= 30; n++ {
		g.record(it, Attempt{N: n}, "", "")
	}
	got := g.dex["spin"].tok.Attempts
	if len(got) != maxAttempts || got[0].N != 30-maxAttempts+1 || got[len(got)-1].N != 30 {
		t.Fatalf("attempts = %+v", got)
	}
	for i := 0; i < volRecent+25; i++ {
		g.record(&item{token: domain.Token{Mint: strconv.Itoa(i)}}, Attempt{N: 1}, "", "")
	}
	if len(g.dex) != volRecent || len(g.dexOrd) != volRecent {
		t.Fatalf("dex=%d ord=%d", len(g.dex), len(g.dexOrd))
	}
}

func TestVolumeTrackLeavesWhenTokenLeavesTiers(t *testing.T) {
	q := &fakeQ{limit: 10, vols: map[string]domain.Volume{
		"stay": vol5m(100),
		"gone": vol5m(100),
	}}
	g := New(Config{MinVolume5m: 1000}, q, quiet())
	book := &memBook{tokens: []domain.Token{{Mint: "stay"}, {Mint: "gone"}}}
	g.recheck(context.Background(), book)
	book.tokens = []domain.Token{{Mint: "stay"}}
	g.recheck(context.Background(), book)
	g.mu.Lock()
	_, stay := g.vol["stay"]
	_, gone := g.vol["gone"]
	g.mu.Unlock()
	if !stay || gone {
		t.Fatalf("stay=%v gone=%v", stay, gone)
	}
}

func TestLowVolumeNeedsThreeSamplesInARow(t *testing.T) {
	q := &fakeQ{limit: 10, vols: map[string]domain.Volume{
		"dying": vol5m(900),
		"alive": vol5m(900),
		"dead":  {},
		"edge":  vol5m(1000),
	}}
	g := New(Config{MinVolume5m: 1000, Recheck: time.Hour}, q, quiet())
	book := &memBook{tokens: []domain.Token{
		{Mint: "dying", Symbol: "D"},
		{Mint: "alive", Symbol: "A"},
		{Mint: "dead", Symbol: "X"},
		{Mint: "edge", Symbol: "E"},
		{Mint: "down", Symbol: "N"},
	}}
	q.failed = []string{"down"}
	g.recheck(context.Background(), book)
	if len(book.tokens) != 5 {
		t.Fatalf("one low sample dropped tokens: %v", book.tokens)
	}

	g.recheck(context.Background(), book)
	if len(book.tokens) != 5 {
		t.Fatalf("two low samples dropped tokens: %v", book.tokens)
	}

	q.vols["alive"] = vol5m(1200)
	q.vols["edge"] = vol5m(1100)
	g.recheck(context.Background(), book)
	if book.has("dying") || book.has("dead") || !book.has("alive") || !book.has("edge") || !book.has("down") {
		t.Fatalf("left = %v", book.tokens)
	}

	q.vols["alive"] = vol5m(10)
	g.recheck(context.Background(), book)
	g.recheck(context.Background(), book)
	if !book.has("alive") {
		t.Fatal("a sample at the floor did not clear the streak")
	}
	g.recheck(context.Background(), book)
	if book.has("alive") {
		t.Fatalf("left = %v", book.tokens)
	}
	if len(book.noted) == 0 || book.noted[0] != vol5m(900) {
		t.Fatalf("noted = %+v", book.noted)
	}
}

func TestUnchangedVolumeDropsOnTheThirdRecheck(t *testing.T) {
	q := &fakeQ{limit: 10, vols: map[string]domain.Volume{
		"stuck": vol5m(17000),
		"moves": vol5m(17000),
		"down":  vol5m(8000),
	}}
	g := New(Config{MinVolume5m: 1000, Recheck: time.Hour}, q, quiet())
	book := &memBook{tokens: []domain.Token{
		{Mint: "stuck", Symbol: "S"},
		{Mint: "moves", Symbol: "M"},
		{Mint: "down", Symbol: "N"},
	}}
	q.failed = []string{"down"}
	g.recheck(context.Background(), book)
	g.recheck(context.Background(), book)
	if len(book.tokens) != 3 {
		t.Fatalf("two identical samples dropped tokens: %v", book.tokens)
	}

	q.vols["moves"] = vol5m(17001)
	g.recheck(context.Background(), book)
	if book.has("stuck") || !book.has("moves") || !book.has("down") {
		t.Fatalf("left = %v", book.tokens)
	}

	g.recheck(context.Background(), book)
	if !book.has("moves") {
		t.Fatal("a one-dollar change did not clear the frozen streak")
	}
	g.recheck(context.Background(), book)
	if book.has("moves") {
		t.Fatalf("left = %v", book.tokens)
	}

	q.failed = nil
	g.recheck(context.Background(), book)
	g.recheck(context.Background(), book)
	if !book.has("down") {
		t.Fatal("a failed request counted as a frozen sample")
	}
	g.recheck(context.Background(), book)
	if book.has("down") {
		t.Fatalf("left = %v", book.tokens)
	}
}

func TestRunAsksOneFreshTokenBeforeARecheck(t *testing.T) {
	q := &fakeQ{vols: map[string]domain.Volume{
		"fresh": vol5m(5000),
		"old":   vol5m(5000),
	}}
	g := New(Config{After: time.Second, MinVolume5m: 1000, Retry: time.Second, Recheck: 30 * time.Second}, q, quiet())
	now := time.Now()
	g.now = func() time.Time { return now }
	g.Add(domain.Token{Mint: "fresh", CreatedAt: now.Add(-time.Minute)})
	book := &memBook{tokens: []domain.Token{{Mint: "old"}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.Run(ctx, func(domain.Token) {}, book)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		q.mu.Lock()
		n := len(q.seq)
		q.mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not stop")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.seq) != 1 || len(q.seq[0]) != 1 || q.seq[0][0] != "fresh" {
		t.Fatalf("calls = %v", q.seq)
	}
	g.mu.Lock()
	tr := g.vol["old"]
	g.mu.Unlock()
	if tr == nil || !tr.next.Equal(now.Add(30*time.Second)) {
		t.Fatalf("recheck scheduled at %v", tr)
	}
}

func TestOverdueRecheckDoesNotWaitForTheEntryQueue(t *testing.T) {
	g := New(Config{MinVolume5m: 1000, Recheck: 30 * time.Second}, &fakeQ{}, quiet())
	now := time.Now()
	book := &memBook{tokens: []domain.Token{{Mint: "old"}}}
	g.mu.Lock()
	g.vol["old"] = &volTrack{next: now.Add(-20 * time.Second)}
	g.pending["retry"] = &item{token: domain.Token{Mint: "retry"}, due: now.Add(-10 * time.Second)}
	g.pending["early"] = &item{token: domain.Token{Mint: "early"}, due: now.Add(-time.Minute)}
	g.mu.Unlock()

	it, _, _ := g.next(now, book)
	if it == nil || it.token.Mint != "early" {
		t.Fatalf("first = %+v, want the ask due a minute ago", it)
	}
	it, tok, ok := g.next(now, book)
	if it != nil || !ok || tok.Mint != "old" {
		t.Fatalf("second = %+v %s %v, want the recheck before the newer retry", it, tok.Mint, ok)
	}
	g.mu.Lock()
	reserved := g.vol["old"].next
	g.mu.Unlock()
	if !reserved.Equal(now.Add(30 * time.Second)) {
		t.Fatalf("recheck reserved until %s", reserved.Sub(now))
	}
	it, _, ok = g.next(now, book)
	if it == nil || it.token.Mint != "retry" || ok {
		t.Fatalf("third = %+v %v", it, ok)
	}
	if it, _, ok := g.next(now, book); it != nil || ok {
		t.Fatalf("nothing should be due: %+v %v", it, ok)
	}
}

func TestOldestDueTokenGoesFirst(t *testing.T) {
	g := New(Config{MinVolume5m: 1000}, &fakeQ{}, quiet())
	now := time.Now()
	g.mu.Lock()
	g.pending["retry"] = &item{token: domain.Token{Mint: "retry"}, due: now.Add(-time.Hour), tries: 4}
	g.pending["fresh"] = &item{token: domain.Token{Mint: "fresh"}, due: now, tries: 0}
	g.pending["older"] = &item{token: domain.Token{Mint: "older"}, due: now.Add(-2 * time.Hour), tries: 0}
	g.mu.Unlock()
	first := g.takeNext(now)
	second := g.takeNext(now)
	third := g.takeNext(now)
	if first == nil || first.token.Mint != "older" || second == nil || second.token.Mint != "retry" || third == nil || third.token.Mint != "fresh" {
		t.Fatalf("order %v %v %v", first, second, third)
	}
}

func TestRecheckDripsOneTokenPerTurn(t *testing.T) {
	g := New(Config{Recheck: 30 * time.Second}, &fakeQ{}, quiet())
	now := time.Now()
	book := &memBook{tokens: []domain.Token{{Mint: "a"}, {Mint: "b"}}}
	if _, ok := g.takeRecheck(now, book); ok {
		t.Fatal("a token just added to the book was asked immediately")
	}
	if _, ok := g.takeRecheck(now, book); ok {
		t.Fatal("the wait was reset")
	}
	now = now.Add(30 * time.Second)
	first, ok := g.takeRecheck(now, book)
	second, ok2 := g.takeRecheck(now, book)
	_, ok3 := g.takeRecheck(now, book)
	if !ok || !ok2 || ok3 || first.Mint != "a" || second.Mint != "b" {
		t.Fatalf("first=%s second=%s ok=%v %v %v", first.Mint, second.Mint, ok, ok2, ok3)
	}
}

func TestClosedStatsPageDoesNotBurnTheRetry(t *testing.T) {
	q := &fakeQ{limit: 10, err: errors.New("axiom stats page is not open"), failed: []string{"m"}}
	g := New(Config{After: time.Second, MinVolume5m: 1000, Retry: 30 * time.Second}, q, quiet())
	now := time.Now()
	g.now = func() time.Time { return now }
	g.Add(domain.Token{Mint: "m", Pool: "PAIR", CreatedAt: now.Add(-time.Minute)})
	g.wave(context.Background(), func(domain.Token) { t.Fatal("emitted") })
	g.mu.Lock()
	it := g.pending["m"]
	paused := g.closedUntil
	g.mu.Unlock()
	if it == nil || it.tries != 0 {
		t.Fatalf("pending %+v", it)
	}
	if !paused.Equal(now.Add(time.Second)) {
		t.Fatalf("paused until %s", paused.Sub(now))
	}
	if g.Health().Requests != 0 {
		t.Fatalf("closed window counted as an attempt: %+v", g.Health())
	}
}

func TestClosedPageLeavesTheRecheckDue(t *testing.T) {
	q := &fakeQ{err: errors.New("axiom stats page is not open")}
	g := New(Config{Recheck: 30 * time.Second}, q, quiet())
	now := time.Now()
	g.now = func() time.Time { return now }
	book := &memBook{tokens: []domain.Token{{Mint: "a"}}}
	g.mu.Lock()
	g.vol["a"] = &volTrack{next: now}
	g.mu.Unlock()
	tok, ok := g.takeRecheck(now, book)
	if !ok || tok.Mint != "a" {
		t.Fatalf("due recheck not taken: %v %s", ok, tok.Mint)
	}
	g.recheckOne(context.Background(), book, tok)
	g.mu.Lock()
	next := g.vol["a"].next
	g.mu.Unlock()
	if !next.Equal(now) {
		t.Fatalf("recheck postponed by %s", next.Sub(now))
	}
}

func TestMigrationDropsTheFrozenCurve(t *testing.T) {
	q := &fakeQ{limit: 10, vols: map[string]domain.Volume{"R": vol5m(50560)}}
	g := New(Config{MinVolume5m: 5000, Recheck: time.Hour}, q, quiet())
	book := &memBook{tokens: []domain.Token{{Mint: "R", Symbol: "RODNEY", Pool: "curve"}}}
	g.recheck(context.Background(), book)
	g.recheck(context.Background(), book)
	g.ResetStreak("R")
	g.recheck(context.Background(), book)
	if !book.has("R") {
		t.Fatal("the same curve volume dropped the token after migration reset the streak")
	}
	g.mu.Lock()
	tr := g.vol["R"]
	g.mu.Unlock()
	if tr == nil || tr.flat != 1 || tr.seen == false {
		t.Fatalf("streak after one fresh sample: %+v", tr)
	}

	g.vol["R"] = &volTrack{flat: 2, seen: true, last: 50560, dropping: true, gen: 4}
	g.ResetStreak("R")
	g.dropWatched(book, domain.Token{Mint: "R", Pool: "curve"}, vol5m(50560), "объём застыл", 4)
	if !book.has("R") {
		t.Fatal("a drop already decided on the curve still removed the token")
	}
}

func TestPendingVolumeFollowsTheNewPair(t *testing.T) {
	q := &fakeQ{limit: 10, vols: map[string]domain.Volume{"R": vol5m(8000)}}
	g := New(Config{After: 10 * time.Second, MinVolume5m: 5000, Retry: time.Second}, q, quiet())
	now := time.Now()
	g.now = func() time.Time { return now }
	g.Add(domain.Token{Mint: "R", Symbol: "RODNEY", Dex: "Pump V1", Pool: "curve", CreatedAt: now})
	if !g.RetargetPending(domain.Token{Mint: "R", Symbol: "RODNEY", Dex: "Pump AMM", Pool: "amm"}) {
		t.Fatal("pending")
	}
	now = now.Add(10 * time.Second)
	var passed domain.Token
	g.wave(context.Background(), func(tok domain.Token) { passed = tok })
	if passed.Pool != "amm" || passed.Dex != "Pump AMM" || len(q.pools) != 1 || q.pools[0] != "amm" {
		t.Fatalf("passed %+v pools %v", passed, q.pools)
	}
}

func TestInFlightCurveAnswerAdmitsTheNewPair(t *testing.T) {
	q := &fakeQ{limit: 10, vols: map[string]domain.Volume{"R": vol5m(8000)}}
	g := New(Config{MinVolume5m: 5000, Retry: time.Second}, q, quiet())
	now := time.Now()
	g.now = func() time.Time { return now }
	curve := domain.Token{Mint: "R", Symbol: "RODNEY", Dex: "Pump V1", Pool: "curve", CreatedAt: now.Add(-time.Minute)}
	g.NoteMigration(domain.Token{Mint: "R", Symbol: "RODNEY", Dex: "Pump AMM", Pool: "amm"})
	var got domain.Token
	g.ask(context.Background(), []*item{{token: curve, born: curve.CreatedAt}}, func(tok domain.Token) { got = tok })
	if got.Pool != "amm" || got.Dex != "Pump AMM" || got.EntryVolume.USD5m != 8000 {
		t.Fatalf("admitted %+v", got)
	}
	g.mu.Lock()
	_, back := g.pending["R"]
	g.mu.Unlock()
	if back {
		t.Fatal("the curve check was queued again")
	}
}

func TestInFlightCurveRefusalDoesNotRetryTheCurve(t *testing.T) {
	q := &fakeQ{limit: 10, vols: map[string]domain.Volume{"R": vol5m(10)}}
	g := New(Config{MinVolume5m: 5000, Retry: time.Second}, q, quiet())
	now := time.Now()
	g.now = func() time.Time { return now }
	curve := domain.Token{Mint: "R", Symbol: "RODNEY", Dex: "Pump V1", Pool: "curve", CreatedAt: now.Add(-time.Minute)}
	g.NoteMigration(domain.Token{Mint: "R", Dex: "Pump AMM", Pool: "amm"})
	g.ask(context.Background(), []*item{{token: curve, born: curve.CreatedAt}}, func(domain.Token) {
		t.Fatal("a curve below the floor admitted the token")
	})
	g.mu.Lock()
	_, back := g.pending["R"]
	g.mu.Unlock()
	if back {
		t.Fatal("the curve was retried after the mint had moved")
	}
}

func TestScreenPairWaitsForTheNewPool(t *testing.T) {
	q := &fakeQ{limit: 10, vols: map[string]domain.Volume{"R": {}}}
	g := New(Config{MinVolume5m: 5000, Retry: time.Second, NoVolumeFor: 2 * time.Minute}, q, quiet())
	now := time.Now()
	g.now = func() time.Time { return now }
	g.ScreenPair(domain.Token{Mint: "R", Dex: "Pump AMM", Pool: "amm", CreatedAt: now.Add(-3 * time.Minute)})
	g.wave(context.Background(), func(domain.Token) { t.Fatal("empty new pair was admitted") })
	g.mu.Lock()
	_, still := g.pending["R"]
	g.mu.Unlock()
	if !still {
		t.Fatal("the curve's age dropped the new pair on the first empty answer")
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
