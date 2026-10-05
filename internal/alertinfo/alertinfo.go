// Package alertinfo completes an alert with everything shown to the user:
// the first theses, the current theses rate, market cap and links.
package alertinfo

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"fomobot/internal/dexscreener"
	"fomobot/internal/domain"
	"fomobot/internal/fomo"
	"fomobot/internal/notify"
)

type Checker interface {
	SortedThesis(ctx context.Context, mint string, after, before time.Time, limit int) (*fomo.TokenThesisPage, error)
}

type PairSource interface {
	Pairs(ctx context.Context, mint string) ([]dexscreener.Pair, error)
}

type LaunchLookup func(mint string) (domain.Token, bool)

type Config struct {
	FirstN     int
	RateWindow time.Duration
	// SearchLimit: theses per request while looking for the first ones.
	SearchLimit int
	// RateLimit: theses per request when counting the recent ones.
	RateLimit         int
	NetworkID         string
	FomoLinkTemplate  string // {mint}, {network}
	AxiomLinkTemplate string // {pair}, {mint}
	// Since: no thesis is older than this (fomo's start).
	Since time.Time
}

type Builder struct {
	cfg      Config
	checkers []Checker
	next     atomic.Uint64
	pairs    PairSource
	lookup   LaunchLookup
	log      *slog.Logger
	now      func() time.Time
}

func New(cfg Config, checkers []Checker, pairs PairSource, lookup LaunchLookup, log *slog.Logger) *Builder {
	if cfg.FirstN <= 0 {
		cfg.FirstN = 3
	}
	if cfg.RateWindow <= 0 {
		cfg.RateWindow = 10 * time.Minute
	}
	if cfg.SearchLimit < cfg.FirstN+1 {
		cfg.SearchLimit = max(500, cfg.FirstN+1)
	}
	if cfg.RateLimit <= 0 {
		cfg.RateLimit = 500
	}
	if cfg.Since.IsZero() {
		cfg.Since = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return &Builder{cfg: cfg, checkers: checkers, pairs: pairs, lookup: lookup, log: log, now: time.Now}
}

// Complete fills the alert. hint is a thesis page already fetched for the
// token, if any; it saves requests when it holds every thesis.
func (b *Builder) Complete(ctx context.Context, a notify.Alert, hint *fomo.TokenThesisPage) notify.Alert {
	started := time.Now()
	if a.DetectedAt.IsZero() {
		a.DetectedAt = b.now()
	}
	a.Chain = ChainName(b.cfg.NetworkID)
	if b.lookup != nil && a.CreatedAt.IsZero() {
		if l, ok := b.lookup(a.Token); ok {
			a.CreatedAt, a.Dex = l.CreatedAt, l.Dex
		}
	}
	if b.cfg.FomoLinkTemplate != "" {
		a.FomoURL = strings.NewReplacer("{mint}", a.Token, "{network}", b.cfg.NetworkID).Replace(b.cfg.FomoLinkTemplate)
	}
	marketStart := time.Now()
	b.addMarket(ctx, &a)
	market := time.Since(marketStart)
	var queries int
	thesesStart := time.Now()
	b.addTheses(ctx, &a, hint, &queries)
	b.log.Info("alert assembled",
		"token", a.Token, "kind", a.Kind, "ticker", a.Symbol,
		"took", time.Since(started).Round(time.Millisecond),
		"market", market.Round(time.Millisecond),
		"theses", time.Since(thesesStart).Round(time.Millisecond),
		"queries", queries, "count", a.Count, "recent", a.Recent,
		"first_exact", a.FirstExact, "mc", a.MarketCap)
	return a
}

func (b *Builder) addMarket(ctx context.Context, a *notify.Alert) {
	pair := ""
	if b.pairs != nil {
		started := time.Now()
		pairs, err := b.pairs.Pairs(ctx, a.Token)
		took := time.Since(started).Round(time.Millisecond)
		if err != nil {
			b.log.Warn("dexscreener pairs", "token", a.Token, "took", took, "err", err)
		} else {
			b.log.Info("dexscreener pairs", "token", a.Token, "took", took, "pairs", len(pairs))
		}
		if best, ok := dexscreener.Best(pairs); ok {
			pair = best.PairAddress
			if a.MarketCap == 0 {
				a.MarketCap = best.MarketCap
			}
		}
		for _, p := range pairs {
			if a.Name == "" && p.TokenName != "" {
				a.Name = p.TokenName
			}
			if a.Symbol == "" && p.TokenSymbol != "" {
				a.Symbol = p.TokenSymbol
			}
		}
	}
	if pair == "" && b.lookup != nil {
		if l, ok := b.lookup(a.Token); ok {
			pair = l.Pool
		}
	}
	if pair == "" {
		pair = a.Token
	}
	if b.cfg.AxiomLinkTemplate != "" {
		a.AxiomURL = strings.NewReplacer("{pair}", pair, "{mint}", a.Token).Replace(b.cfg.AxiomLinkTemplate)
	}
}

func complete(p *fomo.TokenThesisPage) bool {
	return p != nil && len(p.Items) >= p.Count
}

func (b *Builder) addTheses(ctx context.Context, a *notify.Alert, hint *fomo.TokenThesisPage, queries *int) {
	if len(b.checkers) == 0 && !complete(hint) {
		return
	}
	now := a.DetectedAt
	all := hint
	if !complete(all) {
		p, err := b.query(ctx, a.Token, b.cfg.Since, now.Add(time.Minute), b.cfg.SearchLimit, queries)
		if err != nil {
			b.log.Warn("theses for alert", "token", a.Token, "err", err)
			if all == nil {
				return
			}
		} else {
			all = p
		}
	}
	a.CountKnown, a.Count = true, all.Observed()
	a.RateWindow = b.cfg.RateWindow

	if complete(all) {
		b.fromItems(a, all.Items)
		sorted := notify.SortByTime(all.Items)
		a.First = notify.ThesesFrom(sorted[:min(b.cfg.FirstN, len(sorted))])
		a.FirstExact = true
		b.setLatest(a, sorted)
		b.rememberTheses(a, all.Items)
		from := now.Add(-b.cfg.RateWindow)
		a.RateKnown, a.Recent = true, 0
		for _, t := range sorted {
			if !t.CreatedAt.Before(from) {
				a.Recent++
			}
		}
		return
	}

	var recent []fomo.Thesis
	if p, err := b.query(ctx, a.Token, now.Add(-b.cfg.RateWindow), now.Add(time.Minute), b.cfg.RateLimit, queries); err != nil {
		b.log.Warn("recent theses for alert", "token", a.Token, "err", err)
	} else {
		recent = p.Items
		a.RateKnown, a.Recent = true, len(p.Items)
		a.RecentCapped = len(p.Items) >= b.cfg.RateLimit || p.HasNextPage
		b.fromItems(a, p.Items)
		b.setLatest(a, p.Items)
	}
	b.fromItems(a, all.Items)

	first, exact, err := b.searchFirst(ctx, a.Token, now, queries)
	if err != nil {
		b.log.Warn("search first theses", "token", a.Token, "err", err)
		first, exact = notify.SortByTime(all.Items), false
	}
	a.First = notify.ThesesFrom(first[:min(b.cfg.FirstN, len(first))])
	a.FirstExact = exact
	if len(a.Latest) == 0 {
		b.setLatest(a, all.Items)
	}
	b.rememberTheses(a, all.Items, recent, first)
}

// rememberTheses keeps the fetched theses for the open token page.
func (b *Builder) rememberTheses(a *notify.Alert, groups ...[]fomo.Thesis) {
	if len(a.Theses) > 0 {
		return
	}
	seen := map[string]bool{}
	var items []fomo.Thesis
	for _, g := range groups {
		for _, t := range g {
			key := t.CreatedAt.UTC().Format(time.RFC3339Nano) + "\n" + t.Handle + "\n" + t.Comment
			if seen[key] {
				continue
			}
			seen[key] = true
			items = append(items, t)
		}
	}
	if len(items) == 0 {
		return
	}
	a.Theses = notify.ThesesFrom(notify.SortByTime(items))
}

// setLatest keeps the newest theses, oldest first, for the feed row.
func (b *Builder) setLatest(a *notify.Alert, items []fomo.Thesis) {
	if len(items) == 0 || len(a.Latest) > 0 {
		return
	}
	n := b.cfg.FirstN
	if n <= 0 {
		n = 3
	}
	sorted := notify.SortByTime(items)
	if len(sorted) > n {
		sorted = sorted[len(sorted)-n:]
	}
	a.Latest = notify.ThesesFrom(sorted)
}
func (b *Builder) fromItems(a *notify.Alert, items []fomo.Thesis) {
	var latest fomo.Thesis
	for _, t := range items {
		if a.ImageURL == "" && t.TokenImageURL != "" {
			a.ImageURL = t.TokenImageURL
		}
		if a.Symbol == "" && t.Ticker != "" {
			a.Symbol = t.Ticker
		}
		if t.CreatedAt.After(latest.CreatedAt) {
			latest = t
		}
	}
	if a.MarketCap == 0 {
		a.MarketCap = latest.MarketCapAtCreation
	}
}

// searchFirst narrows the window [Since, T] by halving T until it holds
// between FirstN and SearchLimit theses, which then include the first ones.
func (b *Builder) searchFirst(ctx context.Context, mint string, now time.Time, queries *int) ([]fomo.Thesis, bool, error) {
	started := time.Now()
	n, limit := b.cfg.FirstN, b.cfg.SearchLimit
	lo, hi := b.cfg.Since, now.Add(time.Minute)
	steps := 0
	for i := 0; i < 40 && hi.Sub(lo) > time.Second; i++ {
		steps++
		mid := lo.Add(hi.Sub(lo) / 2)
		p, err := b.query(ctx, mint, b.cfg.Since, mid, limit, queries)
		if err != nil {
			b.log.Warn("first theses search failed", "token", mint, "steps", steps,
				"took", time.Since(started).Round(time.Millisecond), "err", err)
			return nil, false, err
		}
		truncated := len(p.Items) >= limit || p.HasNextPage
		switch {
		case truncated:
			hi = mid
		case len(p.Items) < n:
			lo = mid
		default:
			b.log.Info("first theses found", "token", mint, "steps", steps,
				"took", time.Since(started).Round(time.Millisecond), "exact", true, "items", len(p.Items))
			return notify.SortByTime(p.Items), true, nil
		}
	}
	steps++
	p, err := b.query(ctx, mint, b.cfg.Since, hi, limit, queries)
	if err != nil {
		b.log.Warn("first theses search failed", "token", mint, "steps", steps,
			"took", time.Since(started).Round(time.Millisecond), "err", err)
		return nil, false, err
	}
	truncated := len(p.Items) >= limit || p.HasNextPage
	b.log.Info("first theses found", "token", mint, "steps", steps,
		"took", time.Since(started).Round(time.Millisecond), "exact", !truncated, "items", len(p.Items))
	return notify.SortByTime(p.Items), !truncated, nil
}

// maxAccountsPerQuery bounds how many accounts one request may go through
// when accounts fail.
const maxAccountsPerQuery = 3

func (b *Builder) query(ctx context.Context, mint string, after, before time.Time, limit int, queries *int) (*fomo.TokenThesisPage, error) {
	var err error
	for range min(len(b.checkers), maxAccountsPerQuery) {
		*queries++
		c := b.checkers[int(b.next.Add(1)-1)%len(b.checkers)]
		started := time.Now()
		var p *fomo.TokenThesisPage
		p, err = c.SortedThesis(ctx, mint, after, before, limit)
		args := []any{"token", mint, "limit", limit,
			"window", before.Sub(after).Round(time.Second),
			"took", time.Since(started).Round(time.Millisecond),
			"items", itemsOf(p)}
		if err != nil {
			b.log.Warn("alert thesis query", append(args, "err", err)...)
		} else {
			b.log.Info("alert thesis query", args...)
		}
		if err == nil || ctx.Err() != nil {
			return p, err
		}
		if !fomo.Retryable(err) {
			return nil, err
		}
	}
	return nil, err
}

func itemsOf(p *fomo.TokenThesisPage) int {
	if p == nil {
		return 0
	}
	return len(p.Items)
}

func ChainName(networkID string) string {
	switch networkID {
	case fomo.SolanaNetworkID, "":
		return "Solana"
	case "1":
		return "Ethereum"
	case "56":
		return "BNB Chain"
	case "8453":
		return "Base"
	}
	return "network " + networkID
}
