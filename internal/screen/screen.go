// Package screen holds a new token until it is old enough, then asks Axiom
// for the pair's trading volume over the last 5 minutes. At or below
// MinVolume5m the token never reaches the fomo checks. A pair with no volume
// yet is asked again until NoVolumeFor after creation; the first empty answer
// at or past it drops the token. While the token is in tiers 1–3, the
// 5-minute volume is asked again on its own clock. The first sample waits one
// Recheck after the token enters the book. Three samples in a row under
// MinVolume5m drop it. Tier 4 is not checked.
//
// Asks go out one token at a time. Among asks already due, the one that has
// been waiting longest goes first, whether it is the first ask, a retry or a
// tier recheck. The stats window spaces the actual HTTP calls.
package screen

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"time"

	"fomobot/internal/domain"
)

// Querier fetches pair volumes for a wave of tokens. A token with no trades
// yet answers with zero volume. failed are the tokens whose request did not
// complete.
type Querier interface {
	Limit() int
	Wave(ctx context.Context, tokens []domain.Token) (vols map[string]domain.Volume, failed []string, err error)
}

type Config struct {
	After time.Duration
	// MinVolume5m is the USD volume over the last 5 minutes a token must
	// exceed to enter tier 1, and must keep reaching while in tiers 1–3.
	MinVolume5m float64
	Retry       time.Duration
	Recheck     time.Duration
	// NoVolumeFor is how long after creation a pair may answer with no
	// volume. An empty answer at or past it drops the token. A request Axiom
	// did not answer is not an empty answer.
	NoVolumeFor time.Duration
}

type Gate struct {
	cfg Config
	q   Querier
	log *slog.Logger
	now func() time.Time

	mu      sync.Mutex
	pending map[string]*item
	// migrated is the Pump AMM pair for a mint whose first volume read may
	// still be in flight on the bonding curve.
	migrated map[string]domain.Token
	dex      map[string]*volState
	dexOrd   []string
	dexSum   Health
	vol      map[string]*volTrack
	// closedUntil holds the lane when the stats window is not open yet.
	// Reads in that stretch are not attempts and are not paced.
	closedUntil time.Time
}

// volTrack is the recheck streak for one watched token, and when the next
// sample is due. bad counts samples under MinVolume5m. flat counts samples
// whose 5-minute volume is exactly the same as the previous one.
type volTrack struct {
	bad  int
	flat int
	last float64
	seen bool
	next time.Time
	// gen changes when a Pump AMM migration throws the streak away.
	// dropping is set only for the sample that decided to release the token.
	gen      int
	dropping bool
}

type item struct {
	token domain.Token
	due   time.Time
	tries int
	// born is the creation time, or when the token was added if the launch
	// had none or it was in the future. NoVolumeFor counts from here.
	born time.Time
}

// How many screened tokens the health report keeps, newest last in storage.
const volRecent = 200

// maxAttempts is how many Axiom answers one token keeps. A request Axiom did
// not answer is asked again with no limit, so the list is capped here.
const maxAttempts = 8

// lowSamples is how many samples in a row under MinVolume5m drop a token.
// flatSamples is how many rechecks in a row with the exact same 5-minute
// volume do the same. The entry sample is not one of them.
const (
	lowSamples  = 3
	flatSamples = 3
)

// Attempt is one Axiom answer for a token. N starts at 1: the check After
// after the token was created. The next numbers are retries.
type Attempt struct {
	N           int       `json:"n"`
	At          time.Time `json:"at"`
	TokenAgeSec int       `json:"token_age_sec"`
	Answered    bool      `json:"answered"`
	Volume5m    float64   `json:"volume_5m"`
	Trades5m    int       `json:"trades_5m"`
}

// VolToken is every Axiom answer for one mint and the screen's decision.
type VolToken struct {
	Mint     string    `json:"mint"`
	Symbol   string    `json:"symbol,omitempty"`
	Decision string    `json:"decision,omitempty"`
	Why      string    `json:"why,omitempty"`
	Attempts []Attempt `json:"attempts"`
}

// Health says how often volume showed up, and on which request.
type Health struct {
	Tokens            int         `json:"tokens"`
	Requests          int         `json:"requests"`
	VolumeFromRequest map[int]int `json:"volume_from_request"`
	NeverVolume       int         `json:"never_volume"`
	Passed            int         `json:"passed"`
	Dropped           int         `json:"dropped"`
	Waiting           int         `json:"waiting"`
	Recent            []VolToken  `json:"recent"`
}

type volState struct {
	tok       VolToken
	sawVolume bool
	done      bool
}

func New(cfg Config, q Querier, log *slog.Logger) *Gate {
	if cfg.After <= 0 {
		cfg.After = time.Minute
	}
	if cfg.Retry <= 0 {
		cfg.Retry = 30 * time.Second
	}
	if cfg.NoVolumeFor <= 0 {
		cfg.NoVolumeFor = 2 * time.Minute
	}
	if log == nil {
		log = slog.Default()
	}
	return &Gate{
		cfg: cfg, q: q, log: log, now: time.Now, pending: map[string]*item{},
		migrated: map[string]domain.Token{},
		dex:      map[string]*volState{}, vol: map[string]*volTrack{},
		dexSum: Health{VolumeFromRequest: map[int]int{}},
	}
}

// Add remembers a launch until it is old enough for the volume check.
func (g *Gate) Add(token domain.Token) {
	now := g.now()
	due := token.CreatedAt.Add(g.cfg.After)
	if token.CreatedAt.IsZero() || due.Before(now) {
		due = now
	}
	born := token.CreatedAt
	if born.IsZero() || born.After(now) {
		born = now
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.pending[token.Mint]; ok {
		return
	}
	g.pending[token.Mint] = &item{token: token, due: due, born: born}
}

// NoteMigration remembers the Pump AMM pair. A volume read already in flight
// still carries the curve; the admission uses this pair instead.
func (g *Gate) NoteMigration(token domain.Token) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.migrated[token.Mint] = token
}

// pairNow is the pair a volume result must admit, when the mint has migrated.
// g.mu must be held.
func (g *Gate) pairNow(tok domain.Token) domain.Token {
	next, ok := g.migrated[tok.Mint]
	if !ok || next.Pool == "" {
		return tok
	}
	tok.Dex = next.Dex
	tok.Pool = next.Pool
	return tok
}

// RetargetPending points a token still waiting for its first volume read at
// a new pair. The due time stays. False means the mint is not waiting.
func (g *Gate) RetargetPending(token domain.Token) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	it := g.pending[token.Mint]
	if it == nil {
		return false
	}
	it.token.Dex = token.Dex
	it.token.Pool = token.Pool
	g.log.Info("flow", "step", "объём", "decision", "миграция",
		"why", "ждём объём новой пары",
		"token", token.Mint, "ticker", token.Symbol, "pair", token.Pool)
	return true
}

// ScreenPair queues a pair that was not admitted on the bonding curve.
// The no-volume clock starts now, so a just-opened Pump AMM pool gets the
// full wait instead of the curve's age.
func (g *Gate) ScreenPair(token domain.Token) {
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.pending[token.Mint]; ok {
		return
	}
	if st := g.dex[token.Mint]; st != nil && st.done {
		if st.tok.Decision == "пропуск" {
			if g.dexSum.Dropped > 0 {
				g.dexSum.Dropped--
			}
			if !st.sawVolume && g.dexSum.NeverVolume > 0 {
				g.dexSum.NeverVolume--
			}
		}
		st.done = false
		st.tok.Decision = ""
		st.tok.Why = ""
	}
	g.pending[token.Mint] = &item{token: token, due: now, born: now}
}

// ResetStreak forgets volume samples that belong to the pair the token left.
// The next recheck is due immediately and reads whatever pair the book has now.
func (g *Gate) ResetStreak(mint string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	tr := g.vol[mint]
	if tr == nil {
		g.vol[mint] = &volTrack{next: g.now()}
		return
	}
	tr.gen++
	tr.dropping = false
	tr.bad = 0
	tr.flat = 0
	tr.last = 0
	tr.seen = false
	tr.next = g.now()
}

// Run asks for one due token at a time and emits those whose volume clears
// the threshold. book, when set, is rechecked on the same lane. A nil book
// only screens new tokens.
func (g *Gate) Run(ctx context.Context, emit func(domain.Token), book Book) {
	for ctx.Err() == nil {
		if g.now().Before(g.closedUntil) {
			if !g.idle(ctx, book) {
				return
			}
			continue
		}
		it, tok, ok := g.next(g.now(), book)
		switch {
		case it != nil:
			g.ask(ctx, []*item{it}, emit)
		case ok:
			g.recheckOne(ctx, book, tok)
		default:
			if !g.idle(ctx, book) {
				return
			}
		}
	}
}

// next takes the ask that has been due the longest: a first check, a retry,
// or a tier 1–3 recheck. A recheck does not wait for the entry queue to
// empty; when both are due at the same moment the entry queue goes first.
func (g *Gate) next(now time.Time, book Book) (*item, domain.Token, bool) {
	var tok domain.Token
	var at time.Time
	var due bool
	if book != nil {
		tok, at, due = g.dueRecheck(now, book)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	best := g.oldestDue(now)
	if best != nil && (!due || !at.Before(best.due)) {
		delete(g.pending, best.token.Mint)
		return best, domain.Token{}, false
	}
	if !due {
		return nil, domain.Token{}, false
	}
	g.vol[tok.Mint].next = now.Add(g.cfg.Recheck)
	return nil, tok, true
}

// takeNext removes the due token that has been waiting the longest.
// A newer token does not jump ahead of a retry whose turn already came.
func (g *Gate) takeNext(now time.Time) *item {
	g.mu.Lock()
	defer g.mu.Unlock()
	best := g.oldestDue(now)
	if best == nil {
		return nil
	}
	delete(g.pending, best.token.Mint)
	return best
}

// oldestDue is the pending ask whose time came first. g.mu must be held.
func (g *Gate) oldestDue(now time.Time) *item {
	var best *item
	for _, it := range g.pending {
		if it.due.After(now) {
			continue
		}
		if best == nil || it.due.Before(best.due) {
			best = it
		}
	}
	return best
}

// takeRecheck picks one tier 1–3 token whose own recheck time has arrived
// and reserves the next sample.
func (g *Gate) takeRecheck(now time.Time, book Book) (domain.Token, bool) {
	tok, _, ok := g.dueRecheck(now, book)
	if !ok {
		return domain.Token{}, false
	}
	g.mu.Lock()
	g.vol[tok.Mint].next = now.Add(g.cfg.Recheck)
	g.mu.Unlock()
	return tok, true
}

// dueRecheck finds the tier 1–3 token whose recheck has been due the longest
// and when it fell due. It does not reserve the sample. A token just admitted
// to the book waits one full Recheck before its first sample, so the
// admission read is not repeated.
func (g *Gate) dueRecheck(now time.Time, book Book) (domain.Token, time.Time, bool) {
	if g.cfg.Recheck <= 0 {
		return domain.Token{}, time.Time{}, false
	}
	tokens := book.VolumeTokens()
	g.forgetVolume(tokens)
	g.mu.Lock()
	defer g.mu.Unlock()
	var best domain.Token
	var bestAt time.Time
	found := false
	for _, tok := range tokens {
		tr := g.vol[tok.Mint]
		if tr == nil {
			g.vol[tok.Mint] = &volTrack{next: now.Add(g.cfg.Recheck)}
			continue
		}
		if tr.next.IsZero() {
			tr.next = now.Add(g.cfg.Recheck)
			continue
		}
		if tr.next.After(now) {
			continue
		}
		if !found || tr.next.Before(bestAt) {
			best, bestAt, found = tok, tr.next, true
		}
	}
	return best, bestAt, found
}

// idle sleeps until the next due ask, or a short poll so a token added
// meanwhile is noticed. false means ctx is done.
func (g *Gate) idle(ctx context.Context, book Book) bool {
	const poll = 200 * time.Millisecond
	wait := poll
	now := g.now()
	g.mu.Lock()
	for _, it := range g.pending {
		if d := it.due.Sub(now); d > 0 && d < wait {
			wait = d
		}
	}
	if book != nil && g.cfg.Recheck > 0 {
		for _, tr := range g.vol {
			if tr.next.IsZero() {
				continue
			}
			if d := tr.next.Sub(now); d > 0 && d < wait {
				wait = d
			}
		}
	}
	g.mu.Unlock()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (g *Gate) wave(ctx context.Context, emit func(domain.Token)) {
	now := g.now()
	limit := g.q.Limit()
	g.mu.Lock()
	batch := make([]*item, 0, limit)
	for _, it := range g.pending {
		if len(batch) >= limit {
			break
		}
		if !it.due.After(now) {
			batch = append(batch, it)
			delete(g.pending, it.token.Mint)
		}
	}
	g.mu.Unlock()
	g.ask(ctx, batch, emit)
}

func (g *Gate) ask(ctx context.Context, batch []*item, emit func(domain.Token)) {
	if len(batch) == 0 {
		return
	}
	now := g.now()
	tokens := make([]domain.Token, len(batch))
	for i, it := range batch {
		tokens[i] = it.token
	}
	vols, failed, waveErr := g.q.Wave(ctx, tokens)
	if ctx.Err() != nil || pageClosed(waveErr) {
		if pageClosed(waveErr) {
			g.blockFor(time.Second)
		}
		g.mu.Lock()
		for _, it := range batch {
			if _, ok := g.pending[it.token.Mint]; !ok {
				g.pending[it.token.Mint] = it
			}
		}
		g.mu.Unlock()
		if pageClosed(waveErr) {
			g.log.Warn("axiom volume wave", "failed", len(failed), "err", waveErr)
		}
		return
	}
	if waveErr != nil {
		g.log.Warn("axiom volume wave", "failed", len(failed), "err", waveErr)
	}
	failedSet := map[string]bool{}
	for _, mint := range failed {
		failedSet[mint] = true
	}
	for _, it := range batch {
		mint := it.token.Mint
		att := Attempt{N: it.tries + 1, At: now, Answered: true}
		if !it.token.CreatedAt.IsZero() {
			att.TokenAgeSec = int(now.Sub(it.token.CreatedAt).Round(time.Second) / time.Second)
		}
		var v domain.Volume
		decision, why := "", ""
		switch {
		case failedSet[mint]:
			att.Answered = false
			if it.token.Pool == "" && g.noVolumeExpired(it, now) {
				decision, why = "пропуск", "нет адреса пары"+g.noVolumeTail()
			}
		default:
			v = vols[mint]
			att.Volume5m, att.Trades5m = v.USD5m, v.Trades5m
			switch {
			case v.USD5m <= 0:
				if g.noVolumeExpired(it, now) {
					decision, why = "пропуск", "объёма нет"+g.noVolumeTail()
				}
			case v.USD5m <= g.cfg.MinVolume5m:
				decision, why = "пропуск", "объём не выше порога"
			default:
				decision, why = "в работу", "объём выше порога"
			}
		}
		g.mu.Lock()
		admitted := g.pairNow(it.token)
		moved := admitted.Pool != it.token.Pool
		g.mu.Unlock()
		// A curve answer that does not clear the filter is not retried: the
		// Pump AMM pair has its own check. A pass admits that pair.
		if moved && decision != "в работу" {
			g.log.Info("flow", "step", "объём", "decision", "миграция",
				"why", "ответ кривой не решает",
				"token", mint, "ticker", it.token.Symbol, "pair", admitted.Pool)
			continue
		}
		g.record(it, att, decision, why)
		if decision == "" {
			why := "ещё нет объёма"
			if it.token.Pool == "" {
				why = "нет адреса пары"
			} else if failedSet[mint] {
				why = "axiom не ответил"
			}
			g.again(it, why)
			continue
		}
		g.log.Info("flow", "step", "объём", "decision", decision, "why", why,
			"token", mint, "ticker", it.token.Symbol,
			"volume_5m", v.USD5m, "trades_5m", v.Trades5m, "volume_1h", v.USD1h, "request", att.N)
		if decision == "пропуск" {
			continue
		}
		tok := admitted
		tok.EntryVolume = v
		emit(tok)
	}
}

// noVolumeExpired is true once NoVolumeFor has passed since the token was born.
func (g *Gate) noVolumeExpired(it *item, now time.Time) bool {
	born := it.born
	if born.IsZero() {
		born = it.token.CreatedAt
	}
	if born.IsZero() {
		return false
	}
	return now.Sub(born) >= g.cfg.NoVolumeFor
}

func (g *Gate) noVolumeTail() string {
	d := g.cfg.NoVolumeFor
	if d%time.Minute == 0 {
		return fmt.Sprintf(" через %d мин после создания", int(d/time.Minute))
	}
	return " через " + d.Round(time.Second).String() + " после создания"
}

func (g *Gate) record(it *item, att Attempt, decision, why string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.dex[it.token.Mint]
	if st == nil {
		st = &volState{tok: VolToken{Mint: it.token.Mint, Symbol: it.token.Symbol}}
		g.dex[it.token.Mint] = st
		g.dexOrd = append(g.dexOrd, it.token.Mint)
		g.dexSum.Tokens++
	}
	if st.tok.Symbol == "" {
		st.tok.Symbol = it.token.Symbol
	}
	if len(st.tok.Attempts) >= maxAttempts {
		kept := make([]Attempt, maxAttempts-1, maxAttempts)
		copy(kept, st.tok.Attempts[len(st.tok.Attempts)-(maxAttempts-1):])
		st.tok.Attempts = kept
	}
	st.tok.Attempts = append(st.tok.Attempts, att)
	g.dexSum.Requests++
	if att.Volume5m > 0 && !st.sawVolume {
		st.sawVolume = true
		g.dexSum.VolumeFromRequest[att.N]++
	}
	if decision != "" && !st.done {
		st.done = true
		st.tok.Decision = decision
		st.tok.Why = why
		if decision == "в работу" {
			g.dexSum.Passed++
		} else {
			g.dexSum.Dropped++
		}
		if !st.sawVolume {
			g.dexSum.NeverVolume++
		}
	}
	for len(g.dexOrd) > volRecent {
		old := g.dexOrd[0]
		delete(g.dex, old)
		g.dexOrd = g.dexOrd[1:]
	}
	if cap(g.dexOrd) > volRecent*2 {
		fresh := make([]string, len(g.dexOrd))
		copy(fresh, g.dexOrd)
		g.dexOrd = fresh
	}
}

// Health is the Axiom volume section of /health/accounts.
func (g *Gate) Health() Health {
	g.mu.Lock()
	defer g.mu.Unlock()
	h := g.dexSum
	h.VolumeFromRequest = maps.Clone(g.dexSum.VolumeFromRequest)
	h.Recent = make([]VolToken, 0, len(g.dexOrd))
	for i := len(g.dexOrd) - 1; i >= 0; i-- {
		st := g.dex[g.dexOrd[i]]
		if st == nil {
			continue
		}
		if !st.done {
			h.Waiting++
		}
		cp := st.tok
		cp.Attempts = append([]Attempt(nil), st.tok.Attempts...)
		h.Recent = append(h.Recent, cp)
	}
	return h
}

// Book is the set of tokens currently checked on fomo.
type Book interface {
	Tokens() []domain.Token
	// VolumeTokens are the ones still in tiers 1–3.
	VolumeTokens() []domain.Token
	Release(mint string) bool
}

// volumeSink receives the latest Axiom volume for a watched token.
// The watch board implements it. A book that does not is left as it is.
type volumeSink interface {
	NoteVolume(mint string, v domain.Volume, at time.Time)
}

// recheck scores every token still in tiers 1–3. Run does not use it:
// live traffic goes through recheckOne so the book is not dumped at once.
// A missing answer is not a sample and does not change either streak.
func (g *Gate) recheck(ctx context.Context, book Book) {
	tokens := book.VolumeTokens()
	g.forgetVolume(tokens)
	limit := g.q.Limit()
	if limit < 1 {
		limit = len(tokens)
	}
	for len(tokens) > 0 {
		if ctx.Err() != nil {
			return
		}
		n := min(len(tokens), limit)
		batch := tokens[:n]
		tokens = tokens[n:]
		vols, failed, err := g.q.Wave(ctx, batch)
		if ctx.Err() != nil {
			return
		}
		g.score(book, batch, vols, failed, err)
	}
}

func (g *Gate) recheckOne(ctx context.Context, book Book, tok domain.Token) {
	if book != nil && !mintWatched(book.VolumeTokens(), tok.Mint) {
		g.mu.Lock()
		delete(g.vol, tok.Mint)
		g.mu.Unlock()
		return
	}
	vols, failed, err := g.q.Wave(ctx, []domain.Token{tok})
	if ctx.Err() != nil {
		return
	}
	if pageClosed(err) {
		g.blockFor(time.Second)
		g.mu.Lock()
		if tr := g.vol[tok.Mint]; tr != nil {
			tr.next = g.now()
		}
		g.mu.Unlock()
		g.log.Warn("axiom volume recheck", "failed", len(failed), "err", err)
		return
	}
	g.score(book, []domain.Token{tok}, vols, failed, err)
}

func mintWatched(tokens []domain.Token, mint string) bool {
	for _, tok := range tokens {
		if tok.Mint == mint {
			return true
		}
	}
	return false
}

func pageClosed(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not open")
}

func (g *Gate) blockFor(d time.Duration) {
	until := g.now().Add(d)
	if until.After(g.closedUntil) {
		g.closedUntil = until
	}
}

func (g *Gate) score(book Book, batch []domain.Token, vols map[string]domain.Volume, failed []string, err error) {
	if err != nil {
		g.log.Warn("axiom volume recheck", "failed", len(failed), "err", err)
	}
	skip := map[string]bool{}
	for _, mint := range failed {
		skip[mint] = true
	}
	for _, tok := range batch {
		if skip[tok.Mint] {
			continue
		}
		v, ok := vols[tok.Mint]
		if !ok {
			continue
		}
		if sink, ok := book.(volumeSink); ok {
			sink.NoteVolume(tok.Mint, v, g.now())
		}
		g.judgeVolume(book, tok, v)
	}
}

// forgetVolume drops samples for tokens that left tiers 1–3. Release already
// removes a token dropped for low volume; expiry and an alert do not.
func (g *Gate) forgetVolume(live []domain.Token) {
	keep := make(map[string]struct{}, len(live))
	for _, tok := range live {
		keep[tok.Mint] = struct{}{}
	}
	g.mu.Lock()
	for mint := range g.vol {
		if _, ok := keep[mint]; !ok {
			delete(g.vol, mint)
		}
	}
	g.mu.Unlock()
}

// judgeVolume records one answered recheck. A 5-minute volume under
// MinVolume5m, zero included, is a low sample; three in a row drop the
// token. Exactly the floor, or more, clears that streak. The same dollar
// three rechecks in a row drops it too: the window is frozen. Any other
// figure starts that count over. A sample under the floor still counts
// toward the frozen streak, but three low samples win and use their own
// reason.
func (g *Gate) judgeVolume(book Book, tok domain.Token, v domain.Volume) {
	g.mu.Lock()
	tr := g.vol[tok.Mint]
	if tr == nil {
		tr = &volTrack{}
		g.vol[tok.Mint] = tr
	}
	if v.USD5m >= g.cfg.MinVolume5m {
		tr.bad = 0
	} else {
		tr.bad++
	}
	if tr.seen && v.USD5m == tr.last {
		tr.flat++
	} else {
		tr.flat = 1
		tr.last = v.USD5m
		tr.seen = true
	}
	bad, flat := tr.bad, tr.flat
	var gen int
	if bad >= lowSamples || flat >= flatSamples {
		tr.gen++
		tr.dropping = true
		gen = tr.gen
	} else {
		tr.dropping = false
	}
	g.mu.Unlock()
	if bad >= lowSamples {
		g.dropWatched(book, tok, v, "объём за 5 минут ниже порога три замера подряд", gen)
		return
	}
	if flat >= flatSamples {
		g.dropWatched(book, tok, v, "объём за 5 минут не меняется три замера подряд", gen)
	}
}

// pairRelease drops a token only while it is still measured on pool.
// After a migration the book holds the new pair, and a sample from the
// curve must not take the token off the list.
type pairRelease interface {
	ReleasePair(mint, pool string) bool
}

func (g *Gate) dropWatched(book Book, tok domain.Token, v domain.Volume, why string, gen int) {
	g.mu.Lock()
	tr := g.vol[tok.Mint]
	if tr == nil || !tr.dropping || tr.gen != gen {
		g.mu.Unlock()
		return
	}
	g.mu.Unlock()
	released := false
	if pr, ok := book.(pairRelease); ok {
		released = pr.ReleasePair(tok.Mint, tok.Pool)
	} else {
		released = book.Release(tok.Mint)
	}
	if !released {
		g.mu.Lock()
		if cur := g.vol[tok.Mint]; cur != nil && cur.gen == gen {
			cur.dropping = false
		}
		g.mu.Unlock()
		return
	}
	g.mu.Lock()
	if cur := g.vol[tok.Mint]; cur != nil && cur.gen == gen {
		delete(g.vol, tok.Mint)
	}
	g.mu.Unlock()
	g.log.Info("flow", "step", "объём", "decision", "снят",
		"why", why, "token", tok.Mint, "ticker", tok.Symbol,
		"volume_5m", v.USD5m, "trades_5m", v.Trades5m, "volume_1h", v.USD1h,
		"min_volume_5m", g.cfg.MinVolume5m)
}

func (g *Gate) again(it *item, why string) {
	it.tries++
	it.due = g.now().Add(g.cfg.Retry)
	g.mu.Lock()
	if _, ok := g.pending[it.token.Mint]; !ok {
		g.pending[it.token.Mint] = it
	}
	g.mu.Unlock()
	g.log.Info("flow", "step", "объём", "decision", "повтор", "why", why,
		"token", it.token.Mint, "ticker", it.token.Symbol, "try", it.tries, "next", g.cfg.Retry.Round(time.Second))
}
