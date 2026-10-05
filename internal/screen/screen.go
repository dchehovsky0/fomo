// Package screen holds a new token until it is old enough, then asks
// DexScreener for market cap. Below the cap the token never reaches the fomo
// checks. An empty answer is asked again. While the token is in tiers 1–3,
// volume.h24 is compared with the previous sample every Recheck. Two samples
// in a row that grew by less than MinGrowth drop the token. Tier 4 is not checked.
package screen

import (
	"context"
	"log/slog"
	"maps"
	"sync"
	"time"

	"fomobot/internal/dexscreener"
	"fomobot/internal/domain"
)

// Querier fetches quotes for a wave of mints. failed are the mints whose
// request did not complete.
type Querier interface {
	Limit() int
	Wave(ctx context.Context, mints []string) (quotes map[string]dexscreener.Quote, failed []string, err error)
}

type Config struct {
	After        time.Duration
	MinMarketCap float64
	Retry        time.Duration
	Recheck      time.Duration
	// MinGrowth is the minimum fractional rise in volume.h24 between samples.
	// 0.05 means 5%. Growth under it is a bad sample.
	MinGrowth float64
}

type Gate struct {
	cfg Config
	q   Querier
	log *slog.Logger
	now func() time.Time

	mu      sync.Mutex
	pending map[string]*item
	dex     map[string]*dexState
	dexOrd  []string
	dexSum  DexHealth
	vol     map[string]*volTrack
}

// volTrack is the last successful 24h volume and how many bad samples
// followed it in a row.
type volTrack struct {
	last float64
	seen bool
	bad  int
}

type item struct {
	token domain.Token
	due   time.Time
	tries int
}

// How many screened tokens the health report keeps, newest last in storage.
const dexRecent = 200

// maxDexAttempts is how many DexScreener answers one token keeps. An empty
// answer is asked again until a cap arrives, so an uncapped list grows for
// as long as the bot runs.
const maxDexAttempts = 8

// A Pump AMM pool is often missing from DexScreener for a few seconds.
// Those empty answers are asked again on a short interval, and only for
// the first pumpAMMTries. Later misses use the ordinary Retry.
const (
	pumpAMMEvery = 3 * time.Second
	pumpAMMTries = 10
)

// DexAttempt is one DexScreener answer for a token. N starts at 1: the check
// After after the token was created. The next numbers are retries.
type DexAttempt struct {
	N           int       `json:"n"`
	At          time.Time `json:"at"`
	TokenAgeSec int       `json:"token_age_sec"`
	Answered    bool      `json:"answered"`
	Listed      bool      `json:"listed"`
	MarketCap   float64   `json:"market_cap"`
	CapKnown    bool      `json:"cap_known"`
	VolumeUSD   float64   `json:"volume_usd"`
	VolumeKnown bool      `json:"volume_known"`
}

// DexToken is every DexScreener answer for one mint and the screen's decision.
type DexToken struct {
	Mint     string       `json:"mint"`
	Symbol   string       `json:"symbol,omitempty"`
	Decision string       `json:"decision,omitempty"`
	Why      string       `json:"why,omitempty"`
	Attempts []DexAttempt `json:"attempts"`
}

// DexHealth says how often market cap and volume showed up, and on which request.
type DexHealth struct {
	Tokens            int         `json:"tokens"`
	Requests          int         `json:"requests"`
	CapFromRequest    map[int]int `json:"cap_from_request"`
	VolumeFromRequest map[int]int `json:"volume_from_request"`
	NeverCap          int         `json:"never_cap"`
	NeverVolume       int         `json:"never_volume"`
	Passed            int         `json:"passed"`
	Dropped           int         `json:"dropped"`
	Waiting           int         `json:"waiting"`
	Recent            []DexToken  `json:"recent"`
}

type dexState struct {
	tok    DexToken
	sawCap bool
	sawVol bool
	done   bool
}

func New(cfg Config, q Querier, log *slog.Logger) *Gate {
	if cfg.After <= 0 {
		cfg.After = time.Minute
	}
	if cfg.Retry <= 0 {
		cfg.Retry = 30 * time.Second
	}
	if cfg.MinGrowth <= 0 {
		cfg.MinGrowth = 0.05
	}
	if log == nil {
		log = slog.Default()
	}
	return &Gate{
		cfg: cfg, q: q, log: log, now: time.Now, pending: map[string]*item{},
		dex: map[string]*dexState{}, vol: map[string]*volTrack{},
		dexSum: DexHealth{
			CapFromRequest:    map[int]int{},
			VolumeFromRequest: map[int]int{},
		},
	}
}

// Add remembers a launch until it is old enough for the cap check.
func (g *Gate) Add(token domain.Token) {
	now := g.now()
	due := token.CreatedAt.Add(g.cfg.After)
	if token.CreatedAt.IsZero() || due.Before(now) {
		due = now
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.pending[token.Mint]; ok {
		return
	}
	g.pending[token.Mint] = &item{token: token, due: due}
}

// Run checks due tokens in one wave and emits those whose cap clears the threshold.
func (g *Gate) Run(ctx context.Context, emit func(domain.Token)) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		g.wave(ctx, emit)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
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
	if len(batch) == 0 {
		return
	}
	mints := make([]string, len(batch))
	for i, it := range batch {
		mints[i] = it.token.Mint
	}
	quotes, failed, waveErr := g.q.Wave(ctx, mints)
	if ctx.Err() != nil {
		g.mu.Lock()
		for _, it := range batch {
			if _, ok := g.pending[it.token.Mint]; !ok {
				g.pending[it.token.Mint] = it
			}
		}
		g.mu.Unlock()
		return
	}
	if waveErr != nil {
		g.log.Warn("dexscreener wave", "failed", len(failed), "err", waveErr)
	}
	failedSet := map[string]bool{}
	for _, mint := range failed {
		failedSet[mint] = true
	}
	for _, it := range batch {
		mint := it.token.Mint
		att := DexAttempt{N: it.tries + 1, At: now, Answered: true}
		if !it.token.CreatedAt.IsZero() {
			att.TokenAgeSec = int(now.Sub(it.token.CreatedAt).Round(time.Second) / time.Second)
		}
		var liq float64
		var picture string
		decision, why := "", ""
		switch {
		case failedSet[mint]:
			att.Answered = false
		default:
			q, ok := quotes[mint]
			att.Listed = ok
			att.MarketCap = q.MarketCap
			att.CapKnown = ok && q.MarketCap > 0
			att.VolumeUSD = q.VolumeUSD
			att.VolumeKnown = ok && q.VolumeKnown
			liq = q.LiquidityUSD
			picture = q.ImageURL
			switch {
			case !ok || q.MarketCap <= 0:
				// An empty answer is not a rejection. Asked again below.
			case g.cfg.MinMarketCap > 0 && q.MarketCap <= g.cfg.MinMarketCap:
				decision, why = "пропуск", "капа не выше порога"
			default:
				decision, why = "в работу", "капа выше порога"
			}
		}
		g.record(it, att, decision, why)
		if decision == "" {
			why := "ещё нет на dexscreener"
			if failedSet[mint] {
				why = "dexscreener не ответил"
			}
			g.again(it, why)
			continue
		}
		if decision == "пропуск" {
			g.log.Info("flow", "step", "капа", "decision", decision, "why", why,
				"token", mint, "ticker", it.token.Symbol,
				"mc", att.MarketCap, "volume", att.VolumeUSD, "request", att.N)
			continue
		}
		g.log.Info("flow", "step", "капа", "decision", decision, "why", why,
			"token", mint, "ticker", it.token.Symbol,
			"mc", att.MarketCap, "volume", att.VolumeUSD, "request", att.N)
		tok := it.token
		tok.EntryMarketCap = att.MarketCap
		if att.VolumeKnown {
			tok.EntryVolumeUSD = att.VolumeUSD
		}
		tok.EntryLiquidityUSD = liq
		if tok.ImageURL == "" {
			tok.ImageURL = domain.PictureURL(picture)
		}
		emit(tok)
	}
}

func (g *Gate) record(it *item, att DexAttempt, decision, why string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.dex[it.token.Mint]
	if st == nil {
		st = &dexState{tok: DexToken{Mint: it.token.Mint, Symbol: it.token.Symbol}}
		g.dex[it.token.Mint] = st
		g.dexOrd = append(g.dexOrd, it.token.Mint)
		g.dexSum.Tokens++
	}
	if st.tok.Symbol == "" {
		st.tok.Symbol = it.token.Symbol
	}
	if len(st.tok.Attempts) >= maxDexAttempts {
		kept := make([]DexAttempt, maxDexAttempts-1, maxDexAttempts)
		copy(kept, st.tok.Attempts[len(st.tok.Attempts)-(maxDexAttempts-1):])
		st.tok.Attempts = kept
	}
	st.tok.Attempts = append(st.tok.Attempts, att)
	g.dexSum.Requests++
	if att.CapKnown && !st.sawCap {
		st.sawCap = true
		g.dexSum.CapFromRequest[att.N]++
	}
	if att.VolumeKnown && !st.sawVol {
		st.sawVol = true
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
		if !st.sawCap {
			g.dexSum.NeverCap++
		}
		if !st.sawVol {
			g.dexSum.NeverVolume++
		}
	}
	for len(g.dexOrd) > dexRecent {
		old := g.dexOrd[0]
		delete(g.dex, old)
		g.dexOrd = g.dexOrd[1:]
	}
	if cap(g.dexOrd) > dexRecent*2 {
		fresh := make([]string, len(g.dexOrd))
		copy(fresh, g.dexOrd)
		g.dexOrd = fresh
	}
}

// Health is the DexScreener section of /health/accounts.
func (g *Gate) Health() DexHealth {
	g.mu.Lock()
	defer g.mu.Unlock()
	h := g.dexSum
	h.CapFromRequest = maps.Clone(g.dexSum.CapFromRequest)
	h.VolumeFromRequest = maps.Clone(g.dexSum.VolumeFromRequest)
	h.Recent = make([]DexToken, 0, len(g.dexOrd))
	for i := len(g.dexOrd) - 1; i >= 0; i-- {
		st := g.dex[g.dexOrd[i]]
		if st == nil {
			continue
		}
		if !st.done {
			h.Waiting++
		}
		cp := st.tok
		cp.Attempts = append([]DexAttempt(nil), st.tok.Attempts...)
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

// quoteSink receives the latest DexScreener numbers for a watched token.
// The watch board implements it. A book that does not is left as it is.
type quoteSink interface {
	NoteQuote(mint string, marketCap, volume, liquidity float64, volumeKnown bool, image string, at time.Time)
}

// Recheck compares volume.h24 with the previous sample for tokens in tiers 1–3.
// Growth under MinGrowth is a bad sample. Two bad samples in a row remove the
// token. A missing answer is not a sample and does not change the streak.
func (g *Gate) Recheck(ctx context.Context, book Book) {
	every := g.cfg.Recheck
	if every <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.recheck(ctx, book)
		}
	}
}

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
		mints := make([]string, len(batch))
		byMint := make(map[string]domain.Token, len(batch))
		for i, tok := range batch {
			mints[i] = tok.Mint
			byMint[tok.Mint] = tok
		}
		quotes, failed, err := g.q.Wave(ctx, mints)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			g.log.Warn("dexscreener recheck", "failed", len(failed), "err", err)
		}
		skip := map[string]bool{}
		for _, mint := range failed {
			skip[mint] = true
		}
		for _, mint := range mints {
			if skip[mint] {
				continue
			}
			q, ok := quotes[mint]
			if !ok {
				continue
			}
			if sink, ok := book.(quoteSink); ok {
				sink.NoteQuote(mint, q.MarketCap, q.VolumeUSD, q.LiquidityUSD, q.VolumeKnown, q.ImageURL, g.now())
			}
			if !q.VolumeKnown {
				continue
			}
			g.judgeVolume(book, byMint[mint], q.VolumeUSD)
		}
	}
}

// forgetVolume drops samples for tokens that left tiers 1–3. Release already
// removes a token dropped for flat volume; expiry and an alert do not.
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

// judgeVolume records one successful 24h volume. The first sample is only a
// baseline. Later growth under MinGrowth counts as bad; two in a row drop
// the token. A rise at or above MinGrowth clears the streak.
func (g *Gate) judgeVolume(book Book, tok domain.Token, volume float64) {
	g.mu.Lock()
	tr := g.vol[tok.Mint]
	if tr == nil {
		tr = &volTrack{}
		g.vol[tok.Mint] = tr
	}
	if !tr.seen {
		tr.seen = true
		tr.last = volume
		g.mu.Unlock()
		return
	}
	prev := tr.last
	if volumeGrew(prev, volume, g.cfg.MinGrowth) {
		tr.bad = 0
	} else {
		tr.bad++
	}
	tr.last = volume
	bad := tr.bad
	g.mu.Unlock()
	if bad < 2 {
		return
	}
	if book.Release(tok.Mint) {
		g.mu.Lock()
		delete(g.vol, tok.Mint)
		g.mu.Unlock()
		g.log.Info("flow", "step", "объём", "decision", "снят",
			"why", "объём не растёт два замера подряд", "token", tok.Mint, "ticker", tok.Symbol,
			"volume", volume, "prev", prev, "min_growth", g.cfg.MinGrowth)
	}
}

// volumeGrew reports whether next is at least min fraction above prev.
// A zero baseline grows when any volume appears.
func volumeGrew(prev, next, min float64) bool {
	if prev <= 0 {
		return next > 0
	}
	return (next-prev)/prev >= min
}

func (g *Gate) again(it *item, why string) {
	it.tries++
	wait := g.cfg.Retry
	if domain.PumpAMM(it.token.Dex) && it.tries <= pumpAMMTries {
		wait = pumpAMMEvery
	}
	it.due = g.now().Add(wait)
	g.mu.Lock()
	if _, ok := g.pending[it.token.Mint]; !ok {
		g.pending[it.token.Mint] = it
	}
	g.mu.Unlock()
	g.log.Info("flow", "step", "капа", "decision", "повтор", "why", why,
		"token", it.token.Mint, "ticker", it.token.Symbol, "try", it.tries, "next", wait.Round(time.Second))
}
