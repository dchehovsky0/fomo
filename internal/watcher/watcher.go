// Package watcher checks young tokens on fomo until they reach the thesis
// threshold or grow older than the lifetime. Checks are spread over several
// fomo accounts, each with its own client and rate limit.
package watcher

import (
	"container/heap"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fomobot/internal/domain"
	"fomobot/internal/fomo"
	"fomobot/internal/notify"
	"fomobot/internal/store"
)

type Checker interface {
	SortedThesis(ctx context.Context, mint string, after, before time.Time, limit int) (*fomo.TokenThesisPage, error)
}

type Account struct {
	Name   string
	Client Checker
	// Workers is how many requests of this account may be in flight; its
	// client's rate limiter still caps the request rate.
	Workers int
}

type Notifier interface {
	Send(ctx context.Context, a notify.Alert) error
}

type Store interface {
	IsSignaled(token string) bool
	MarkSignaled(sig store.Signal) error
}

// TierBook stores the current tier of a watched token and each time it changed.
// EnterTier writes tier 1, or returns the row already stored for this mint.
// AdvanceTier appends a later tier. SetWatchStatus does not touch the tier
// or its history.
type TierBook interface {
	EnterTier(ctx context.Context, tok domain.Token, enteredAt time.Time) (tier int, entered time.Time, status string, inserted bool, err error)
	AdvanceTier(ctx context.Context, mint string, tier int, at time.Time) error
	SetWatchStatus(ctx context.Context, mint, status string, at time.Time) error
}

// Tier: tokens younger than MaxAge are checked every Every.
type Tier struct {
	MaxAge time.Duration
	Every  time.Duration
}

type Config struct {
	Lifetime time.Duration
	// VolumeFor: DexScreener volume applies while the token has been in work
	// for less than this. Zero means the whole lifetime.
	VolumeFor   time.Duration
	MinTheses   int
	ThesisLimit int
	Schedule    []Tier
	RetryDelay  time.Duration
	// AuthPause: how long an account rests after fomo rejected its token.
	AuthPause time.Duration
	// Complete adds market data, the first theses and links to an alert;
	// page is the check that reached the threshold.
	Complete func(ctx context.Context, a notify.Alert, page *fomo.TokenThesisPage) notify.Alert
}

type Stats struct {
	Added      atomic.Int64
	Checks     atomic.Int64
	Errors     atomic.Int64
	AuthErrors atomic.Int64
	Signals    atomic.Int64
	SendErrors atomic.Int64
	Expired    atomic.Int64
	// CheckNanos / CheckSamples time a finished thesis check, including the
	// wait for a token and the rate limit, up to the fomo response.
	CheckNanos   atomic.Int64
	CheckSamples atomic.Int64
}

type item struct {
	l            domain.Token
	due          time.Time
	idx          int
	checks       int
	count        int
	gone         bool
	addedAt      time.Time
	tierMu       sync.Mutex
	tier         int
	tierRetry    time.Time
	sawBelow     bool
	belowAt      time.Time
	belowCount   int
	scheduledDue time.Time
	every        time.Duration
	fails        int
	failKind     string
	failSpans    []span
	// seen is the last fomo answer for this token: the texts the board
	// shows when the operator opens it. The check goroutine writes it
	// under mu; Board does not read it.
	seen   []seenThesis
	seenAt time.Time
	image  string
	// marketCap, volumeUSD and liquidityUSD are the latest DexScreener
	// numbers. They start as the entry snapshot and move on each recheck
	// while the token is still in tiers 1–3.
	marketCap    float64
	volumeUSD    float64
	liquidityUSD float64
	quoteAt      time.Time
	// fomoMS is how long the last fomo request for this token took.
	fomoMS int64
}

// span is a stretch of time lost to one failed request or failed send,
// up to the moment the token could be checked again.
type span struct{ from, to time.Time }

// At most this many failure spans are kept per token; older ones are merged.
const maxFailSpans = 256

type Watcher struct {
	cfg      Config
	accounts []Account
	notifier Notifier
	store    Store
	tiers    TierBook
	log      *slog.Logger
	now      func() time.Time
	Stats    Stats
	perAcc   []atomic.Int64
	// gap spaces only the first check of tokens that arrive together.
	// Later checks keep the tier interval, so a 2s tier stays 2s.
	gap   time.Duration
	phase int

	mu     sync.Mutex
	items  map[string]*item
	queue  itemHeap
	lagSum time.Duration
	lagN   int
	lagMax time.Duration
	wake   chan struct{}
	delays []Delay
}

func New(cfg Config, accounts []Account, n Notifier, st Store, log *slog.Logger) *Watcher {
	if cfg.ThesisLimit <= 0 {
		cfg.ThesisLimit = 500
	}
	if cfg.RetryDelay <= 0 {
		cfg.RetryDelay = 30 * time.Second
	}
	if cfg.AuthPause <= 0 {
		cfg.AuthPause = time.Minute
	}
	slices.SortFunc(cfg.Schedule, func(a, b Tier) int { return int(a.MaxAge - b.MaxAge) })
	return &Watcher{
		cfg: cfg, accounts: accounts, notifier: n, store: st, log: log, now: time.Now,
		perAcc: make([]atomic.Int64, len(accounts)),
		gap:    queueGap(cfg.Schedule),
		items:  map[string]*item{}, wake: make(chan struct{}, 1),
	}
}

// queueGap spreads a wave without stretching one token's own tier interval.
// The cap is 200ms: a short tier (2s) still starts several tokens inside its
// period, just not in the same instant. A test interval of a few milliseconds
// keeps a smaller gap so the token's next check is not pushed back.
func queueGap(schedule []Tier) time.Duration {
	gap := 200 * time.Millisecond
	for _, t := range schedule {
		if t.Every > 0 && t.Every/2 < gap {
			gap = t.Every / 2
		}
	}
	return gap
}

// Add starts watching a launch; the first check is due immediately.
// The tier clock starts here, when the token enters tier 1, not at creation.
func (w *Watcher) Add(l domain.Token) {
	now := w.now()
	if w.store.IsSignaled(l.Mint) {
		w.log.Info("flow", "step", "обработали", "decision", "пропуск", "why", "алерт по этому токену уже был",
			"token", l.Mint, "ticker", l.Symbol)
		return
	}
	w.mu.Lock()
	if _, ok := w.items[l.Mint]; ok {
		w.mu.Unlock()
		w.log.Info("flow", "step", "обработали", "decision", "пропуск", "why", "уже следим за этим токеном",
			"token", l.Mint, "ticker", l.Symbol)
		return
	}
	w.mu.Unlock()
	it := &item{
		l: l, due: now, addedAt: now,
		marketCap: l.EntryMarketCap, volumeUSD: l.EntryVolumeUSD, liquidityUSD: l.EntryLiquidityUSD,
	}
	if l.EntryMarketCap > 0 || l.EntryVolumeUSD > 0 || l.EntryLiquidityUSD > 0 {
		it.quoteAt = now
	}
	if !w.syncTier(it, now) {
		w.log.Info("flow", "step", "обработали", "decision", "пропуск", "why", "наблюдение этого токена уже закончено",
			"token", l.Mint, "ticker", l.Symbol)
		return
	}
	w.mu.Lock()
	if _, ok := w.items[l.Mint]; ok {
		w.mu.Unlock()
		w.log.Info("flow", "step", "обработали", "decision", "пропуск", "why", "уже следим за этим токеном",
			"token", l.Mint, "ticker", l.Symbol)
		return
	}
	it.due = w.firstDue(w.now())
	w.items[l.Mint] = it
	heap.Push(&w.queue, it)
	w.mu.Unlock()
	w.Stats.Added.Add(1)
	w.poke()
}

// NoteQuote stores the latest DexScreener numbers for a token still being
// watched. The entry snapshot on the token stays as it was at admission.
func (w *Watcher) NoteQuote(mint string, marketCap, volume, liquidity float64, volumeKnown bool, image string, at time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	it := w.items[mint]
	if it == nil || it.gone {
		return
	}
	if marketCap > 0 {
		it.marketCap = marketCap
	}
	if volumeKnown {
		it.volumeUSD = volume
	}
	if liquidity > 0 {
		it.liquidityUSD = liquidity
	}
	if picture := domain.PictureURL(image); picture != "" && it.image == "" && it.l.ImageURL == "" {
		it.l.ImageURL = picture
	}
	it.quoteAt = at
}

// SetTiers turns on tier marks. Without it the watch runs as before.
func (w *Watcher) SetTiers(b TierBook) { w.tiers = b }

// Tokens are the launches currently watched.
func (w *Watcher) Tokens() []domain.Token {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]domain.Token, 0, len(w.items))
	for _, it := range w.items {
		out = append(out, it.l)
	}
	return out
}

// Release stops watching mint. A check already in flight is not turned into
// another one.
func (w *Watcher) Release(mint string) bool {
	w.mu.Lock()
	it, ok := w.items[mint]
	if !ok {
		w.mu.Unlock()
		return false
	}
	it.gone = true
	delete(w.items, mint)
	if it.idx >= 0 {
		heap.Remove(&w.queue, it.idx)
	}
	w.mu.Unlock()
	// A token dropped for flat volume does not move to the next tier.
	w.markStatus(mint, "dropped", w.now())
	return true
}

func (w *Watcher) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := range w.accounts {
		for range max(w.accounts[i].Workers, 1) {
			wg.Go(func() { w.worker(ctx, i) })
		}
	}
	wg.Wait()
}

func (w *Watcher) worker(ctx context.Context, acc int) {
	for {
		if ctx.Err() != nil {
			return
		}
		it, ok := w.next(ctx)
		if !ok {
			return
		}
		if pause, err := w.check(ctx, acc, it); pause > 0 {
			w.log.Warn("fomo account is not working, pausing it", "account", w.accounts[acc].Name, "pause", pause, "err", err)
			if sleep(ctx, pause) != nil {
				return
			}
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// next blocks until an item is due and takes it out of the queue.
func (w *Watcher) next(ctx context.Context) (*item, bool) {
	for {
		// A due item is returned before the wait below. Without this check,
		// shutdown puts the token back as already due and the worker pops it
		// again in a tight loop, logging a request it will never send.
		if ctx.Err() != nil {
			return nil, false
		}
		w.mu.Lock()
		var wait time.Duration = -1
		if w.queue.Len() > 0 {
			top := w.queue[0]
			now := w.now()
			if !top.due.After(now) {
				heap.Pop(&w.queue)
				more := w.queue.Len() > 0 && !w.queue[0].due.After(now)
				w.mu.Unlock()
				if more {
					w.poke()
				}
				return top, true
			}
			wait = top.due.Sub(now)
		}
		w.mu.Unlock()

		var t *time.Timer
		var timer <-chan time.Time
		if wait >= 0 {
			t = time.NewTimer(wait)
			timer = t.C
		}
		select {
		case <-ctx.Done():
		case <-w.wake:
		case <-timer:
		}
		if t != nil {
			t.Stop()
		}
		if ctx.Err() != nil {
			return nil, false
		}
	}
}

func (w *Watcher) poke() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *Watcher) reschedule(it *item, due time.Time) {
	w.mu.Lock()
	if it.gone || w.items[it.l.Mint] != it {
		w.mu.Unlock()
		return
	}
	it.due = due
	if it.idx >= 0 {
		heap.Fix(&w.queue, it.idx)
	} else {
		heap.Push(&w.queue, it)
	}
	w.mu.Unlock()
	w.poke()
}

// firstDue staggers tokens that enter on the same wave. The offset stays
// inside the first tier's interval, and later checks are not moved by it.
// The caller holds w.mu.
func (w *Watcher) firstDue(now time.Time) time.Time {
	every := w.interval(0)
	if every <= 0 || w.gap <= 0 {
		return now
	}
	slots := int(every / w.gap)
	if slots < 1 {
		slots = 1
	}
	off := time.Duration(w.phase%slots) * w.gap
	w.phase++
	return now.Add(off)
}

func (w *Watcher) drop(it *item) {
	w.mu.Lock()
	it.gone = true
	delete(w.items, it.l.Mint)
	w.mu.Unlock()
}

// check runs one request for the item and decides what happens to it next.
// A failure of the account itself (no login, rejected token, dead proxy)
// hands the item straight to the other accounts and returns how long this
// account should rest.
func (w *Watcher) check(ctx context.Context, acc int, it *item) (time.Duration, error) {
	now := w.now()
	w.mu.Lock()
	lag := now.Sub(it.due)
	w.lagSum += lag
	w.lagN++
	w.lagMax = max(w.lagMax, lag)
	w.mu.Unlock()

	if ctx.Err() != nil {
		w.reschedule(it, it.due)
		return 0, nil
	}

	end := w.workEnd(it)
	if w.cfg.Lifetime > 0 && now.After(end) {
		w.Stats.Expired.Add(1)
		w.drop(it)
		w.noteTier(it, now)
		w.markStatus(it.l.Mint, "expired", now)
		w.logCheck(acc, it, 0, lag, "expired", it.count, 0, nil, nil)
		return 0, nil
	}

	w.log.Info("flow", "step", "fomo", "status", "запрос",
		"account", w.accounts[acc].Name, "token", it.l.Mint, "ticker", it.l.Symbol, "check", it.checks+1)
	started := time.Now()
	page, err := w.accounts[acc].Client.SortedThesis(ctx, it.l.Mint, it.l.CreatedAt.Add(-time.Minute), now.Add(time.Minute), w.cfg.ThesisLimit)
	took := time.Since(started)
	w.noteFomo(it, took)
	finished := w.now()
	w.Stats.CheckNanos.Add(int64(took))
	w.Stats.CheckSamples.Add(1)
	if err != nil {
		if ctx.Err() != nil {
			w.reschedule(it, it.due)
			return 0, nil
		}
		w.Stats.Errors.Add(1)
		switch {
		case fomo.AuthRejected(err):
			w.Stats.AuthErrors.Add(1)
			w.miss(it, "auth", started, time.Now())
			w.logCheck(acc, it, took, lag, "auth", it.count, w.cfg.AuthPause, err, nil)
			w.noteTier(it, finished)
			w.reschedule(it, finished)
			return w.cfg.AuthPause, err
		case fomo.Retryable(err):
			w.miss(it, "retry", started, time.Now())
			w.logCheck(acc, it, took, lag, "retry", it.count, w.cfg.RetryDelay, err, nil)
			w.noteTier(it, finished)
			// Not immediately: the tier interval is the soonest this token is
			// asked again. An immediate handoff let every account hit it at once.
			w.reschedule(it, nextDue(now, finished, w.interval(w.inWork(it, finished))))
			return w.cfg.RetryDelay, err
		}
		w.miss(it, "error", started, later(time.Now(), started.Add(w.cfg.RetryDelay)))
		w.logCheck(acc, it, took, lag, "error", it.count, w.cfg.RetryDelay, err, nil)
		w.noteTier(it, finished)
		w.reschedule(it, finished.Add(w.cfg.RetryDelay))
		return 0, nil
	}
	w.Stats.Checks.Add(1)
	w.perAcc[acc].Add(1)
	// count on this endpoint trails the items that are already in the body.
	// Alerting on the field waits another poll or two after the thesis arrived.
	seenN := page.Observed()
	seen := time.Now()
	// publishPage is the only checks++ for this answer. The board reads it
	// under mu, and the log below runs after that write.
	w.publishPage(it, page, seen)

	if seenN >= w.cfg.MinTheses {
		w.logCheck(acc, it, took, lag, "threshold", seenN, 0, nil, page)
		w.signal(ctx, it, page, finished, it.due, started, seen, took)
		return 0, nil
	}
	it.sawBelow = true
	it.belowAt = seen
	it.belowCount = seenN
	w.clearMiss(it)
	every := w.interval(w.inWork(it, finished))
	next := nextDue(now, finished, every)
	if w.cfg.Lifetime > 0 && next.After(end) {
		if !end.After(finished) {
			w.Stats.Expired.Add(1)
			w.drop(it)
			w.noteTier(it, finished)
			w.markStatus(it.l.Mint, "expired", finished)
			w.logCheck(acc, it, took, lag, "expired", seenN, 0, nil, page)
			return 0, nil
		}
		next = end // one last look right at the end of the lifetime
	}
	it.scheduledDue = next
	it.every = next.Sub(finished)
	w.logCheck(acc, it, took, lag, "below", seenN, next.Sub(finished), nil, page)
	w.noteTier(it, finished)
	w.reschedule(it, next)
	return 0, nil
}

// nextDue is one poll period after the check started. A check that already
// lasted longer than its tier interval would otherwise be put back on the
// queue as already due, and the next free account would take it at once.
func nextDue(started, finished time.Time, every time.Duration) time.Time {
	if every <= 0 {
		every = time.Minute
	}
	next := started.Add(every)
	if !next.After(finished) {
		return finished.Add(every)
	}
	return next
}

func (w *Watcher) logCheck(acc int, it *item, took, lag time.Duration, result string, count int, next time.Duration, err error, page *fomo.TokenThesisPage) {
	alert, why := "не отправлен", ""
	switch result {
	case "below":
		why = "тезисов меньше порога"
	case "threshold":
		alert, why = "собираем", "тезисов достаточно"
	case "expired":
		why = "срок наблюдения вышел"
	case "auth":
		why = "аккаунт не пустил, токен уйдёт другому"
	case "retry":
		why = "fomo временно недоступен, повторим"
	default:
		why = "ошибка запроса, повторим"
	}
	args := []any{
		"step", "fomo", "status", "ответ", "alert", alert, "why", why,
		"account", w.accounts[acc].Name, "token", it.l.Mint, "ticker", it.l.Symbol,
		"count", count, "need", w.cfg.MinTheses, "checks", it.checks,
		"took", took.Round(time.Millisecond), "lag", lag.Round(time.Millisecond),
		"token_age", w.now().Sub(it.l.CreatedAt).Round(time.Second),
	}
	if next > 0 {
		args = append(args, "next_in", next.Round(time.Second))
	}
	if page != nil {
		args = append(args, "items", len(page.Items), "api_count", page.Count)
	}
	if err != nil {
		w.log.Warn("flow", append(args, "err", err)...)
		return
	}
	w.log.Info("flow", args...)
}

// tierNumber is 1 for the first schedule band, 2 for the next, and so on.
func (w *Watcher) tierNumber(age time.Duration) int {
	for i, t := range w.cfg.Schedule {
		if age < t.MaxAge {
			return i + 1
		}
	}
	if n := len(w.cfg.Schedule); n > 0 {
		return n
	}
	return 1
}

// syncTier writes the current tier. It returns false when the database already
// has a finished watch for this mint, so the token is not taken again.
// A failed write is retried later. While the stored tier is current, this
// does not talk to the database.
func (w *Watcher) syncTier(it *item, now time.Time) bool {
	if w.tiers == nil {
		return true
	}
	it.tierMu.Lock()
	defer it.tierMu.Unlock()
	if now.Before(it.tierRetry) && it.tier == 0 {
		return true
	}
	n := w.tierNumber(w.inWork(it, now))
	if n == it.tier && it.tier != 0 {
		return true
	}
	if now.Before(it.tierRetry) {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if it.tier == 0 {
		tier, entered, status, inserted, err := w.tiers.EnterTier(ctx, it.l, it.addedAt)
		if err != nil {
			it.tierRetry = now.Add(30 * time.Second)
			w.log.Warn("watch tier", "token", it.l.Mint, "tier", 1, "err", err)
			return true
		}
		if !inserted {
			if status != "watching" {
				return false
			}
			if !entered.IsZero() {
				// Board reads addedAt under mu. This is the only write after
				// the item is published, and it happens on a restore from the
				// database. mu is taken after tierMu; Board never takes tierMu.
				w.mu.Lock()
				it.addedAt = entered
				w.mu.Unlock()
			}
			it.tier = tier
			n = w.tierNumber(w.inWork(it, now))
		} else {
			it.tier = 1
		}
	}
	for it.tier < n {
		next := it.tier + 1
		if err := w.tiers.AdvanceTier(ctx, it.l.Mint, next, w.tierStarted(it, next, now)); err != nil {
			it.tierRetry = now.Add(30 * time.Second)
			w.log.Warn("watch tier", "token", it.l.Mint, "tier", next, "err", err)
			return true
		}
		it.tier = next
	}
	it.tierRetry = time.Time{}
	return true
}

func (w *Watcher) noteTier(it *item, now time.Time) { w.syncTier(it, now) }

// tierStarted is when this tier begins. Tier 1 begins at entry. Later tiers
// begin at the previous tier's age limit, but not in the future.
func (w *Watcher) tierStarted(it *item, tier int, now time.Time) time.Time {
	if tier <= 1 || tier-2 >= len(w.cfg.Schedule) {
		return now
	}
	at := it.addedAt.Add(w.cfg.Schedule[tier-2].MaxAge)
	if at.After(now) {
		return now
	}
	return at
}

func (w *Watcher) markStatus(mint, status string, at time.Time) {
	if w.tiers == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := w.tiers.SetWatchStatus(ctx, mint, status, at); err != nil {
		w.log.Warn("watch tier status", "token", mint, "status", status, "err", err)
	}
}

func (w *Watcher) interval(age time.Duration) time.Duration {
	for _, t := range w.cfg.Schedule {
		if age < t.MaxAge {
			return t.Every
		}
	}
	if n := len(w.cfg.Schedule); n > 0 {
		return w.cfg.Schedule[n-1].Every
	}
	return time.Minute
}

func (w *Watcher) miss(it *item, kind string, from, to time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	it.fails++
	it.failKind = kind
	if to.After(from) {
		it.failSpans = append(it.failSpans, span{from, to})
	}
	if len(it.failSpans) > maxFailSpans {
		it.failSpans[1].from = it.failSpans[0].from
		it.failSpans = it.failSpans[1:]
	}
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func (w *Watcher) signal(ctx context.Context, it *item, page *fomo.TokenThesisPage, now, due, started, seen time.Time, fomoTook time.Duration) {
	ticker := ""
	for _, t := range page.Items {
		if t.Ticker != "" {
			ticker = t.Ticker
			break
		}
	}
	if ticker == "" {
		ticker = it.l.Symbol
	}
	n := page.Observed()
	rec := store.Signal{Token: it.l.Mint, Ticker: ticker, Count: n, SentAt: now}

	alert := notify.Alert{
		Kind: notify.KindTheses, Threshold: w.cfg.MinTheses, Token: it.l.Mint, Symbol: ticker, Name: it.l.Name,
		CreatedAt: it.l.CreatedAt, Dex: it.l.Dex, DetectedAt: now, CountKnown: true, Count: n,
		Tier: w.tierNumber(w.inWork(it, now)), FomoMS: nonNegMS(fomoTook),
	}
	asmStart := time.Now()
	if w.cfg.Complete != nil {
		alert = w.cfg.Complete(ctx, alert, page)
	}
	assembled := time.Since(asmStart)
	sendStart := time.Now()
	if err := w.notifier.Send(ctx, alert); err != nil {
		w.Stats.SendErrors.Add(1)
		w.log.Error("flow", "step", "алерт", "alert", "не отправлен", "why", "отправка не удалась, повторим",
			"where", "theses", "token", it.l.Mint, "ticker", alert.Symbol, "count", n,
			"assembled", assembled.Round(time.Millisecond), "send", time.Since(sendStart).Round(time.Millisecond), "err", err)
		w.miss(it, "send", started, later(time.Now(), started.Add(w.cfg.RetryDelay)))
		w.noteTier(it, now)
		w.reschedule(it, now.Add(w.cfg.RetryDelay))
		return
	}
	sentAt := time.Now()
	sent := sentAt.Sub(sendStart)
	if err := w.store.MarkSignaled(rec); err != nil {
		w.log.Error("save signal", "token", it.l.Mint, "err", err)
	}
	w.Stats.Signals.Add(1)
	d := w.delay(it, page, due, started, seen, sentAt, fomoTook, assembled, sent)
	w.log.Info("flow", "step", "алерт", "alert", "отправлен", "where", "theses",
		"token", it.l.Mint, "ticker", alert.Symbol, "count", n,
		"token_age", now.Sub(it.l.CreatedAt).Round(time.Second), "checks", it.checks,
		"assembled", assembled.Round(time.Millisecond), "send", sent.Round(time.Millisecond),
		"took", time.Since(asmStart).Round(time.Millisecond), "why", d.Why)
	w.remember(d)
	w.drop(it)
	w.noteTier(it, now)
	w.markStatus(it.l.Mint, "alerted", sentAt)
}

// Snapshot is the state of the watcher for periodic logs.
type Snapshot struct {
	Watching int
	Overdue  int // due more than a minute ago
	// DemandRPS is the request rate the schedule asks for with the tokens
	// watched now; compare it with the accounts' combined rate limit.
	DemandRPS  float64
	AvgLag     time.Duration
	MaxLag     time.Duration
	PerAccount map[string]int64
}

// Snapshot also resets the lag counters.
func (w *Watcher) Snapshot() Snapshot {
	now := w.now()
	w.mu.Lock()
	s := Snapshot{Watching: len(w.items), MaxLag: w.lagMax, PerAccount: map[string]int64{}}
	if w.lagN > 0 {
		s.AvgLag = w.lagSum / time.Duration(w.lagN)
	}
	w.lagSum, w.lagN, w.lagMax = 0, 0, 0
	s.DemandRPS = w.demandLocked(now)
	for _, it := range w.queue {
		if now.Sub(it.due) > time.Minute {
			s.Overdue++
		}
	}
	w.mu.Unlock()
	for i, a := range w.accounts {
		s.PerAccount[a.Name] = w.perAcc[i].Load()
	}
	return s
}

// workEnd is when tier 4 runs out. The clock starts when the token entered
// tier 1, which is when watching started.
func (w *Watcher) workEnd(it *item) time.Time {
	start := it.addedAt
	if start.IsZero() {
		start = it.l.CreatedAt
	}
	return start.Add(w.cfg.Lifetime)
}

func (w *Watcher) inWork(it *item, now time.Time) time.Duration {
	start := it.addedAt
	if start.IsZero() {
		start = it.l.CreatedAt
	}
	return now.Sub(start)
}

// VolumeTokens are the tokens still in tiers 1–3, the ones whose 24h volume
// is still checked. Tier 4 is past VolumeFor and is left out.
func (w *Watcher) VolumeTokens() []domain.Token {
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]domain.Token, 0, len(w.items))
	limit := w.cfg.VolumeFor
	if limit <= 0 {
		limit = w.cfg.Lifetime
	}
	for _, it := range w.items {
		if w.inWork(it, now) < limit {
			out = append(out, it.l)
		}
	}
	return out
}

// Watching is how many tokens are in the fomo checks right now.
// It does not reset the lag counters Snapshot uses for the stats log.
func (w *Watcher) Watching() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.items)
}

// DemandRPS is the request rate the schedule asks for with the tokens watched
// now. It does not reset the lag counters Snapshot uses for the stats log.
func (w *Watcher) DemandRPS() float64 {
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.demandLocked(now)
}

func (w *Watcher) demandLocked(now time.Time) float64 {
	var demand float64
	for _, it := range w.items {
		every := w.interval(w.inWork(it, now))
		if every > 0 {
			demand += 1 / every.Seconds()
		}
	}
	return demand
}

// itemHeap orders by due time; among equally due items the younger token goes first.
type itemHeap []*item

func (h itemHeap) Len() int { return len(h) }
func (h itemHeap) Less(i, j int) bool {
	if !h[i].due.Equal(h[j].due) {
		return h[i].due.Before(h[j].due)
	}
	return h[i].l.CreatedAt.After(h[j].l.CreatedAt)
}
func (h itemHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].idx = i
	h[j].idx = j
}
func (h *itemHeap) Push(x any) {
	it := x.(*item)
	it.idx = len(*h)
	*h = append(*h, it)
}
func (h *itemHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	old[n-1] = nil
	it.idx = -1
	*h = old[:n-1]
	return it
}

// How many sent alerts /health/alerts keeps, newest first.
const delayKeep = 100

// Delay explains the gap between the thesis that crossed the threshold and
// the moment the alert was sent. The millisecond parts sum to gap_ms.
type Delay struct {
	Token       string    `json:"token"`
	Symbol      string    `json:"symbol"`
	Threshold   int       `json:"threshold"`
	Count       int       `json:"count"`
	Items       int       `json:"items_with_time"`
	HasNext     bool      `json:"has_next_page"`
	ThesisAt    time.Time `json:"thesis_at,omitzero"`
	PrevAt      time.Time `json:"prev_at,omitzero"`
	PrevCount   int       `json:"prev_count"`
	AddedAt     time.Time `json:"added_at,omitzero"`
	ScheduledAt time.Time `json:"scheduled_at,omitzero"`
	DueAt       time.Time `json:"due_at,omitzero"`
	StartedAt   time.Time `json:"started_at,omitzero"`
	SeenAt      time.Time `json:"seen_at"`
	SentAt      time.Time `json:"sent_at"`
	EveryMS     int64     `json:"every_ms"`
	Fails       int       `json:"fails"`
	FailKind    string    `json:"fail_kind,omitempty"`
	// Parts of gap_ms, in time order.
	NotWatchedMS int64  `json:"not_watched_ms"`
	BlindMS      int64  `json:"fomo_blind_ms"`
	SlotMS       int64  `json:"slot_ms"`
	FailMS       int64  `json:"fail_ms"`
	LateMS       int64  `json:"late_ms"`
	FomoMS       int64  `json:"fomo_ms"`
	AssembleMS   int64  `json:"assemble_ms"`
	SendMS       int64  `json:"send_ms"`
	OtherMS      int64  `json:"other_ms"`
	GapMS        int64  `json:"gap_ms"`
	Why          string `json:"why"`

	failSpans []span
}

func (w *Watcher) delay(it *item, page *fomo.TokenThesisPage, due, started, seen, sent time.Time, fomoTook, assembled, send time.Duration) Delay {
	d := Delay{
		Token: it.l.Mint, Symbol: it.l.Symbol, Threshold: w.cfg.MinTheses,
		AddedAt: it.addedAt, DueAt: due, StartedAt: started, SeenAt: seen, SentAt: sent,
		Fails: it.fails, FailKind: it.failKind, failSpans: it.failSpans,
		FomoMS: nonNegMS(fomoTook), AssembleMS: nonNegMS(assembled), SendMS: nonNegMS(send),
	}
	if page != nil {
		d.Count = page.Observed()
		d.HasNext = page.HasNextPage
		for _, t := range page.Items {
			if !t.CreatedAt.IsZero() {
				d.Items++
			}
		}
	}
	if at, ok := thresholdThesisAt(page, w.cfg.MinTheses); ok {
		d.ThesisAt = at
	}
	if it.sawBelow {
		d.PrevAt = it.belowAt
		d.PrevCount = it.belowCount
		d.ScheduledAt = it.scheduledDue
		d.EveryMS = it.every.Milliseconds()
	}
	fillParts(&d)
	d.Why = explain(d)
	return d
}

// fillParts splits sent-thesis into measured pieces. The time between the
// mark and the start of the request that saw the threshold is split by what
// the token was doing: not yet added, fomo still answering below the
// threshold, waiting for its scheduled slot, failed requests or sends, and
// the rest, which is the queue. other_ms is whatever the pieces do not cover.
func fillParts(d *Delay) {
	if d.ThesisAt.IsZero() || d.StartedAt.IsZero() || d.SeenAt.IsZero() || d.SentAt.IsZero() {
		return
	}
	d.GapMS = d.SentAt.Sub(d.ThesisAt).Milliseconds()
	start := d.StartedAt
	if !start.After(d.ThesisAt) {
		// The mark is inside the request that saw the threshold.
		d.FomoMS = nonNegMS(d.SeenAt.Sub(d.ThesisAt))
	} else {
		cursor := d.ThesisAt
		if d.PrevAt.IsZero() && d.AddedAt.After(cursor) {
			d.NotWatchedMS = nonNegMS(earlier(d.AddedAt, start).Sub(cursor))
			cursor = earlier(d.AddedAt, start)
		}
		if d.PrevAt.After(cursor) {
			d.BlindMS = nonNegMS(earlier(d.PrevAt, start).Sub(cursor))
			cursor = earlier(d.PrevAt, start)
		}
		if d.ScheduledAt.After(cursor) {
			d.SlotMS = nonNegMS(earlier(d.ScheduledAt, start).Sub(cursor))
			cursor = earlier(d.ScheduledAt, start)
		}
		var fail time.Duration
		for _, s := range d.failSpans {
			from, to := later(s.from, cursor), earlier(s.to, start)
			if to.After(from) {
				fail += to.Sub(from)
			}
		}
		d.FailMS = nonNegMS(fail)
		d.LateMS = nonNegMS(start.Sub(cursor) - fail)
	}
	spent := d.NotWatchedMS + d.BlindMS + d.SlotMS + d.FailMS + d.LateMS + d.FomoMS + d.AssembleMS + d.SendMS
	d.OtherMS = d.GapMS - spent
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (w *Watcher) remember(d Delay) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.delays = append([]Delay{d}, w.delays...)
	if len(w.delays) > delayKeep {
		w.delays = w.delays[:delayKeep]
	}
}

// Delays are the recent alerts, newest first, with why each one left when it did.
func (w *Watcher) Delays() []Delay {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Delay(nil), w.delays...)
}

// DelayHandler serves Delays as JSON.
func (w *Watcher) DelayHandler() http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		h := rw.Header()
		h.Set("Content-Type", "application/json; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		alerts := w.Delays()
		if alerts == nil {
			alerts = []Delay{}
		}
		enc := json.NewEncoder(rw)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		_ = enc.Encode(struct {
			Alerts []Delay `json:"alerts"`
		}{Alerts: alerts})
	})
}

// thresholdThesisAt is the time of the nth thesis only when the response
// contains every thesis. A newest-first page with a next page does not.
func thresholdThesisAt(page *fomo.TokenThesisPage, n int) (time.Time, bool) {
	if page == nil || n <= 0 {
		return time.Time{}, false
	}
	times := make([]time.Time, 0, len(page.Items))
	for _, t := range page.Items {
		if !t.CreatedAt.IsZero() {
			times = append(times, t.CreatedAt)
		}
	}
	if len(times) < n || page.HasNextPage || page.Count > len(times) {
		return time.Time{}, false
	}
	slices.SortFunc(times, time.Time.Compare)
	return times[n-1], true
}

func explain(d Delay) string {
	n := d.Threshold
	if n <= 0 {
		n = 3
	}
	if d.ThesisAt.IsZero() {
		return fmt.Sprintf("Время %d-го тезиса неизвестно: fomo сообщил %d тезисов, со временем в ответе %d, есть следующая страница: %v. Метка есть только когда ответ содержит все тезисы, иначе причину разрыва не называю.",
			n, d.Count, d.Items, d.HasNext)
	}
	lead := dominantCause(d)
	return lead + " " + equation(d)
}

// A part shorter than this is not named as a cause, only shown in the sum.
const causeMin = time.Second

func dominantCause(d Delay) string {
	n := d.Threshold
	ms := func(v int64) string { return shortDur(time.Duration(v) * time.Millisecond) }
	var lines []string
	if d.NotWatchedMS >= causeMin.Milliseconds() {
		lines = append(lines, fmt.Sprintf("%d-й тезис появился до того, как токен попал в работу (%s, ждал проверку DexScreener): %s",
			n, clock(d.AddedAt), ms(d.NotWatchedMS)))
	}
	if d.BlindMS >= causeMin.Milliseconds() {
		lines = append(lines, fmt.Sprintf("fomo в %s ещё отдал %d тезисов при пороге %d, хотя %d-й уже был помечен в %s: %s. Это ответ API, не очередь",
			clock(d.PrevAt), d.PrevCount, n, n, clock(d.ThesisAt), ms(d.BlindMS)))
	}
	if d.SlotMS >= causeMin.Milliseconds() {
		every := ""
		if d.EveryMS > 0 {
			every = " (каждые " + ms(d.EveryMS) + ")"
		}
		lines = append(lines, fmt.Sprintf("ждали опрос по расписанию%s, назначенный на %s: %s",
			every, clock(d.ScheduledAt), ms(d.SlotMS)))
	}
	if d.FailMS >= causeMin.Milliseconds() {
		lines = append(lines, fmt.Sprintf("неудачные попытки (%d, последняя: %s) и паузы после них: %s",
			d.Fails, failName(d.FailKind), ms(d.FailMS)))
	}
	if d.LateMS >= causeMin.Milliseconds() {
		lines = append(lines, fmt.Sprintf("очередь: запрос начался в %s, ждал свободный аккаунт %s",
			clock(d.StartedAt), ms(d.LateMS)))
	}
	if d.FomoMS >= causeMin.Milliseconds() {
		lines = append(lines, "медленный ответ fomo: "+ms(d.FomoMS))
	}
	if d.AssembleMS >= causeMin.Milliseconds() {
		lines = append(lines, "сборка алерта: "+ms(d.AssembleMS))
	}
	if d.SendMS >= causeMin.Milliseconds() {
		lines = append(lines, "отправка в Telegram: "+ms(d.SendMS))
	}
	if d.OtherMS >= causeMin.Milliseconds() || d.OtherMS <= -causeMin.Milliseconds() {
		lines = append(lines, "не разложено: "+ms(d.OtherMS)+" (проверь часы и лог)")
	}
	if len(lines) == 0 {
		return "Причина: задержек дольше секунды нет."
	}
	return "Причина: " + strings.Join(lines, "; ") + "."
}

func failName(kind string) string {
	switch kind {
	case "auth":
		return "аккаунт не пустил"
	case "retry":
		return "fomo ответил 429/5xx или сеть"
	case "error":
		return "ошибка запроса, пауза перед повтором"
	case "send":
		return "Telegram не принял алерт"
	default:
		return kind
	}
}

func equation(d Delay) string {
	var terms []string
	add := func(v int64, label string) {
		if v != 0 {
			terms = append(terms, fmt.Sprintf("%s %s", label, shortDur(time.Duration(v)*time.Millisecond)))
		}
	}
	add(d.NotWatchedMS, "не в работе")
	add(d.BlindMS, "fomo не показывал порог")
	add(d.SlotMS, "ждали слот")
	add(d.FailMS, "ошибки")
	add(d.LateMS, "очередь")
	add(d.FomoMS, "запрос fomo")
	add(d.AssembleMS, "сборка")
	add(d.SendMS, "отправка")
	add(d.OtherMS, "прочее")
	gap := shortDur(time.Duration(d.GapMS) * time.Millisecond)
	if len(terms) == 0 {
		return "Разрыв " + gap + "."
	}
	return "Разрыв " + gap + " = " + strings.Join(terms, " + ") + "."
}

func nonNegMS(d time.Duration) int64 {
	if d < 0 {
		return 0
	}
	return d.Milliseconds()
}

func clock(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.In(time.Local).Format("15:04:05")
}

func shortDur(d time.Duration) string {
	if d < 0 {
		return "-" + shortDur(-d)
	}
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
