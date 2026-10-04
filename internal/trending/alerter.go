package trending

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"fomobot/internal/fomo"
	"fomobot/internal/notify"
)

type Notifier interface {
	Send(ctx context.Context, a notify.Alert) error
}

type Store interface {
	TrendingBaselined() bool
	SetTrendingBaselined() error
	TrendingLastSeen(mint string) (time.Time, bool)
	TouchTrending(mint string, at time.Time)
	MarkTrendingAlerted(mint string, at time.Time) error
}

type Config struct {
	NetworkID string
	// RealertAfter: a token absent from trending for longer than this is
	// announced again when it returns. Zero: once per token.
	RealertAfter time.Duration
	// SilentFirstSnapshot: on the very first start the current list is
	// recorded without alerts.
	SilentFirstSnapshot bool
	RetryDelay          time.Duration
	MaxAttempts         int
	// Complete adds the theses, market data and links to an alert.
	Complete func(ctx context.Context, a notify.Alert, page *fomo.TokenThesisPage) notify.Alert
	// Skip drops a token before any fomo request or alert. mint is the token,
	// launchpad is the name fomo sent with the trending row.
	Skip func(mint, launchpad string) bool
}

type Stats struct {
	Alerts     atomic.Int64
	Baseline   atomic.Int64
	SendErrors atomic.Int64
	Lost       atomic.Int64
}

type job struct {
	tok      Token
	returned bool
	at       time.Time
}

type Alerter struct {
	cfg      Config
	store    Store
	notifier Notifier
	log      *slog.Logger
	now      func() time.Time
	queue    chan job
	Stats    Stats

	mu      sync.Mutex
	pending map[string]bool
}

func NewAlerter(cfg Config, st Store, n Notifier, log *slog.Logger) *Alerter {
	if cfg.RetryDelay <= 0 {
		cfg.RetryDelay = 30 * time.Second
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 5
	}
	return &Alerter{cfg: cfg, store: st, notifier: n, log: log,
		now: time.Now, queue: make(chan job, 1024), pending: map[string]bool{}}
}

// Handle takes a trending message; alerts are sent by Run.
func (a *Alerter) Handle(msg Message) {
	if msg.Kind == KindRemove {
		return
	}
	now := a.now()
	if a.cfg.SilentFirstSnapshot && !a.store.TrendingBaselined() {
		if msg.Kind != KindSnapshot {
			return
		}
		n := 0
		for _, t := range msg.Tokens {
			if t.NetworkID == a.cfg.NetworkID {
				a.store.TouchTrending(t.Mint, now)
				n++
			}
		}
		if err := a.store.SetTrendingBaselined(); err != nil {
			a.log.Error("save trending baseline", "err", err)
		}
		a.Stats.Baseline.Add(int64(n))
		a.log.Info("first start: tokens already trending are recorded without alerts", "tokens", n)
		return
	}
	for _, t := range msg.Tokens {
		if t.NetworkID != a.cfg.NetworkID {
			continue
		}
		if a.cfg.Skip != nil && a.cfg.Skip(t.Mint, t.Launchpad) {
			a.store.TouchTrending(t.Mint, now)
			a.log.Info("flow", "step", "обработали", "decision", "пропуск", "where", "trending",
				"why", "протокол исключён", "token", t.Mint, "ticker", t.Symbol, "launchpad", t.Launchpad)
			continue
		}
		last, known := a.store.TrendingLastSeen(t.Mint)
		isNew := !known || a.cfg.RealertAfter > 0 && now.Sub(last) > a.cfg.RealertAfter
		a.mu.Lock()
		if a.pending[t.Mint] {
			a.mu.Unlock()
			continue
		}
		if !isNew {
			a.mu.Unlock()
			a.store.TouchTrending(t.Mint, now)
			continue
		}
		a.pending[t.Mint] = true
		a.mu.Unlock()
		select {
		case a.queue <- job{tok: t, returned: known, at: now}:
			a.log.Info("flow", "step", "увидели", "where", "trending",
				"token", t.Mint, "ticker", t.Symbol, "rank", t.Rank, "returned", known, "mc", t.MarketCap)
		default:
			a.unpend(t.Mint)
			a.log.Warn("flow", "step", "алерт", "alert", "не отправлен", "where", "trending",
				"why", "очередь полная, повторим со следующим сообщением", "token", t.Mint, "ticker", t.Symbol)
		}
	}
}

func (a *Alerter) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-a.queue:
			if ctx.Err() != nil {
				return
			}
			a.alert(ctx, j)
		}
	}
}

func (a *Alerter) alert(ctx context.Context, j job) {
	defer a.unpend(j.tok.Mint)
	t := j.tok
	alert := notify.Alert{
		Kind: notify.KindTrending, Rank: t.Rank, Returned: j.returned,
		Token: t.Mint, Symbol: t.Symbol, Name: t.Name, ImageURL: t.ImageURL, MarketCap: t.MarketCap,
		DetectedAt: j.at,
	}
	a.log.Info("flow", "step", "fomo", "status", "запрос", "where", "trending",
		"token", t.Mint, "ticker", t.Symbol, "rank", t.Rank)
	asmStart := time.Now()
	if a.cfg.Complete != nil {
		alert = a.cfg.Complete(ctx, alert, nil)
	}
	assembled := time.Since(asmStart)
	for attempt := 1; ; attempt++ {
		sendStart := time.Now()
		err := a.notifier.Send(ctx, alert)
		sent := time.Since(sendStart)
		if err == nil {
			if err := a.store.MarkTrendingAlerted(t.Mint, a.now()); err != nil {
				a.log.Error("save trending alert", "token", t.Mint, "err", err)
			}
			a.Stats.Alerts.Add(1)
			a.log.Info("flow", "step", "алерт", "alert", "отправлен", "where", "trending",
				"token", t.Mint, "ticker", t.Symbol, "rank", t.Rank, "count", alert.Count, "returned", j.returned,
				"queued_for", a.now().Sub(j.at).Round(time.Millisecond),
				"assembled", assembled.Round(time.Millisecond), "send", sent.Round(time.Millisecond))
			return
		}
		if ctx.Err() != nil {
			return
		}
		a.Stats.SendErrors.Add(1)
		if attempt >= a.cfg.MaxAttempts {
			a.Stats.Lost.Add(1)
			a.log.Error("flow", "step", "алерт", "alert", "не отправлен", "where", "trending",
				"why", "исчерпали попытки", "token", t.Mint, "ticker", t.Symbol, "attempts", attempt, "err", err)
			a.store.TouchTrending(t.Mint, a.now())
			return
		}
		a.log.Warn("flow", "step", "алерт", "alert", "не отправлен", "where", "trending",
			"why", "отправка не удалась, повторим", "token", t.Mint, "ticker", t.Symbol, "attempt", attempt,
			"assembled", assembled.Round(time.Millisecond), "send", sent.Round(time.Millisecond), "err", err)
		if sleep(ctx, a.cfg.RetryDelay) != nil {
			return
		}
	}
}

func (a *Alerter) unpend(mint string) {
	a.mu.Lock()
	delete(a.pending, mint)
	a.mu.Unlock()
}
