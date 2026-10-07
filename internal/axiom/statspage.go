package axiom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"fomobot/internal/domain"
	"fomobot/internal/session"
)

const pairStatsURL = "https://api7.axiom.trade/pair-stats-v4"

// One pair-stats read at a time. Parallel reads from one window reached
// HTTP 429 within a minute, and the real ceiling is not published.
// The gap starts at statsGapStart, never goes under statsGapFloor, and doubles
// up to statsGapCeil after a 429, a 404, or a failed fetch. A run of ordinary
// 200s eases it back down.
const (
	statsGapStart  = 500 * time.Millisecond
	statsGapFloor  = 250 * time.Millisecond
	statsGapCeil   = 8 * time.Second
	statsEaseEvery = 25
)

// Ask is one pair whose volume we want, keyed back by Mint.
type Ask struct {
	Mint string `json:"mint"`
	Pair string `json:"pair"`
}

// StatsPage is a second Chrome on axiom.trade, logged into its own account.
// Pair-stats reads run here so they do not share a rate limit with the
// new_pairs socket.
type StatsPage struct {
	cfg Config
	log *slog.Logger

	mu   sync.Mutex
	page context.Context
	call sync.Mutex

	paceMu sync.Mutex
	gap    time.Duration
	next   time.Time
	streak int
}

func NewStatsPage(cfg Config, log *slog.Logger) (*StatsPage, error) {
	if strings.TrimSpace(cfg.Chrome.ProfileDir) == "" {
		return nil, errors.New("axiom: no stats profile")
	}
	dir, err := filepath.Abs(cfg.Chrome.ProfileDir)
	if err != nil {
		return nil, err
	}
	cfg.Chrome.ProfileDir = dir
	// This window's login is the one saved by -axiom-stats-login. The pair
	// stream's cookies belong to the other account and must not be written here.
	cfg.Cookies = nil
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = 90 * time.Second
	}
	return &StatsPage{cfg: cfg, log: log, gap: statsGapStart}, nil
}

// Run keeps the stats window open until ctx is done, restarting Chrome when it
// closes. Volumes waits until the page is up.
func (c *StatsPage) Run(ctx context.Context) {
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := c.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > 5*time.Minute {
			backoff = 5 * time.Second
		}
		c.log.Warn("axiom stats page stopped, restarting chrome", "err", err,
			"up_for", time.Since(start).Round(time.Second), "retry_in", backoff)
		if sleep(ctx, backoff) != nil {
			return
		}
		backoff = min(backoff*2, 2*time.Minute)
	}
}

func (c *StatsPage) session(ctx context.Context) error {
	sessionStart := time.Now()
	browserCtx, closeTab, err := session.OpenTab(ctx, c.cfg.Chrome)
	if err != nil {
		return fmt.Errorf("start chrome: %w", err)
	}
	defer closeTab()
	if err := chromedp.Run(browserCtx); err != nil {
		return fmt.Errorf("start chrome: %w", err)
	}
	startCtx, cancelStart := context.WithTimeout(browserCtx, c.cfg.StartTimeout)
	err = c.open(startCtx)
	cancelStart()
	if err != nil {
		return err
	}
	c.setPage(browserCtx)
	defer c.setPage(nil)
	c.paceMu.Lock()
	gap := c.gap
	c.paceMu.Unlock()
	c.log.Info("axiom stats page opened", "url", c.cfg.AppURL,
		"profile", c.cfg.Chrome.ProfileDir, "took", time.Since(sessionStart).Round(time.Millisecond),
		"gap", gap)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-browserCtx.Done():
		return errors.New("chrome closed")
	}
}

func (c *StatsPage) open(ctx context.Context) error {
	session.HideHeadless(ctx)
	return chromedp.Run(ctx, chromedp.Navigate(c.cfg.AppURL))
}

func (c *StatsPage) setPage(ctx context.Context) {
	c.mu.Lock()
	c.page = ctx
	c.mu.Unlock()
}

// Limit is one. Callers may still pass a list; Volumes sends it one pair at a time.
func (c *StatsPage) Limit() int { return 1 }

// Volumes reads pair-stats for each ask from the stats window.
// vols holds mints the tab answered, including a pair with no trades yet
// (zero volume). failed holds mints whose request did not complete.
func (c *StatsPage) Volumes(ctx context.Context, asks []Ask) (vols map[string]domain.Volume, failed []string, err error) {
	vols = map[string]domain.Volume{}
	if len(asks) == 0 {
		return vols, nil, nil
	}
	c.call.Lock()
	defer c.call.Unlock()
	for len(asks) > 0 {
		if ctx.Err() != nil {
			for _, a := range asks {
				failed = append(failed, a.Mint)
			}
			return vols, failed, ctx.Err()
		}
		a := asks[0]
		asks = asks[1:]
		if strings.TrimSpace(a.Pair) == "" {
			failed = append(failed, a.Mint)
			continue
		}
		c.mu.Lock()
		open := c.page != nil
		c.mu.Unlock()
		if !open {
			failed = append(failed, a.Mint)
			for _, rest := range asks {
				failed = append(failed, rest.Mint)
			}
			return vols, failed, errPageClosed
		}
		if err := c.waitTurn(ctx); err != nil {
			failed = append(failed, a.Mint)
			for _, rest := range asks {
				failed = append(failed, rest.Mint)
			}
			return vols, failed, err
		}
		got, bad, callErr := c.statsBatch(ctx, []Ask{a})
		for mint, v := range got {
			vols[mint] = v
		}
		failed = append(failed, bad...)
		if callErr != nil && err == nil {
			err = callErr
		}
	}
	return vols, failed, err
}

// waitTurn spaces the start of each pair-stats read. The first one goes
// immediately. The gap used here is the one in force before this read.
func (c *StatsPage) waitTurn(ctx context.Context) error {
	c.paceMu.Lock()
	if c.gap <= 0 {
		c.gap = statsGapStart
	}
	wait := time.Until(c.next)
	gap := c.gap
	c.paceMu.Unlock()
	if wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	c.paceMu.Lock()
	c.next = time.Now().Add(gap)
	c.paceMu.Unlock()
	return nil
}

func (c *StatsPage) observeRows(rows []pageRow) {
	for _, row := range rows {
		if row.Error != "" {
			c.slow(row.Error)
			continue
		}
		if row.Status == 200 {
			c.ease()
			continue
		}
		c.slow(fmt.Sprintf("HTTP %d", row.Status))
	}
}

func (c *StatsPage) slow(why string) {
	c.paceMu.Lock()
	defer c.paceMu.Unlock()
	if c.gap <= 0 {
		c.gap = statsGapStart
	}
	next := min(c.gap*2, statsGapCeil)
	c.streak = 0
	later := time.Now().Add(next)
	if later.After(c.next) {
		c.next = later
	}
	if next == c.gap {
		return
	}
	c.gap = next
	c.log.Warn("axiom stats pace", "gap", c.gap, "why", why)
}

func (c *StatsPage) ease() {
	c.paceMu.Lock()
	defer c.paceMu.Unlock()
	if c.gap <= 0 {
		c.gap = statsGapStart
	}
	if c.gap <= statsGapFloor {
		c.streak = 0
		return
	}
	c.streak++
	if c.streak < statsEaseEvery {
		return
	}
	c.streak = 0
	next := c.gap * 4 / 5
	if next < statsGapFloor {
		next = statsGapFloor
	}
	if next == c.gap {
		return
	}
	c.gap = next
	c.log.Info("axiom stats pace", "gap", c.gap)
}

func (c *StatsPage) statsBatch(ctx context.Context, asks []Ask) (map[string]domain.Volume, []string, error) {
	var ready []Ask
	var failed []string
	for _, a := range asks {
		if a.Pair == "" {
			failed = append(failed, a.Mint)
			continue
		}
		ready = append(ready, a)
	}
	if len(ready) == 0 {
		return map[string]domain.Volume{}, failed, nil
	}
	c.mu.Lock()
	page := c.page
	c.mu.Unlock()
	if page == nil {
		for _, a := range ready {
			failed = append(failed, a.Mint)
		}
		return map[string]domain.Volume{}, failed, errPageClosed
	}
	rows, err := evalStats(ctx, page, ready)
	if err != nil {
		if !strings.Contains(err.Error(), "not open") {
			c.slow(err.Error())
		}
		for _, a := range ready {
			failed = append(failed, a.Mint)
		}
		return map[string]domain.Volume{}, failed, err
	}
	c.observeRows(rows)
	vols, bad, err := volumesFromRows(rows)
	seen := map[string]bool{}
	for mint := range vols {
		seen[mint] = true
	}
	for _, mint := range bad {
		seen[mint] = true
	}
	for _, a := range ready {
		if !seen[a.Mint] {
			bad = append(bad, a.Mint)
			if err == nil {
				err = fmt.Errorf("axiom: no answer")
			}
		}
	}
	failed = append(failed, bad...)
	return vols, failed, err
}

// errPageClosed is returned when a stats read is asked before the window is up,
// or after Chrome has gone away. It is not a rate-limit signal.
var errPageClosed = errors.New("axiom stats page is not open")

type pageRow struct {
	Mint   string `json:"mint"`
	Status int    `json:"status"`
	Body   string `json:"body"`
	Error  string `json:"error"`
}

func evalStats(ctx, page context.Context, asks []Ask) ([]pageRow, error) {
	payload, err := json.Marshal(asks)
	if err != nil {
		return nil, err
	}
	base, err := json.Marshal(pairStatsURL + "?pairAddress=")
	if err != nil {
		return nil, err
	}
	script := fmt.Sprintf(statsScript, payload, base)
	runCtx, cancel := context.WithTimeout(page, 20*time.Second)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	var rows []pageRow
	err = chromedp.Run(runCtx, chromedp.Evaluate(script, &rows, awaitStats))
	if err != nil {
		return nil, fmt.Errorf("axiom: %w", err)
	}
	return rows, nil
}

func awaitStats(p *runtime.EvaluateParams) *runtime.EvaluateParams {
	return p.WithAwaitPromise(true).WithTimeout(15_000)
}

const statsScript = `(async () => {
  const asks = %s;
  const base = %s;
  const out = [];
  await Promise.all(asks.map(async (a) => {
    try {
      const r = await fetch(base + encodeURIComponent(a.pair) + "&v=2", {credentials: "include", signal: AbortSignal.timeout(10000)});
      out.push({mint: a.mint, status: r.status, body: await r.text()});
    } catch (e) {
      out.push({mint: a.mint, error: String(e && e.message || e)});
    }
  }));
  return out;
})()`

// volumesFromRows turns the tab's answers into volumes and failures. A 200
// with windows null or all zero is an answer with zero volume, not a failure.
func volumesFromRows(rows []pageRow) (map[string]domain.Volume, []string, error) {
	vols := map[string]domain.Volume{}
	var failed []string
	var first error
	for _, row := range rows {
		if row.Mint == "" {
			continue
		}
		if row.Error != "" {
			failed = append(failed, row.Mint)
			if first == nil {
				first = fmt.Errorf("axiom: %s", row.Error)
			}
			continue
		}
		v, err := volumeFromBody(row.Status, row.Body)
		if err != nil {
			failed = append(failed, row.Mint)
			if first == nil {
				first = err
			}
			continue
		}
		vols[row.Mint] = v
	}
	return vols, failed, first
}

// statsBody is pair-stats-v4. Windows come in the order the site reads them:
// 5m, 1h, 6h, 24h. Each is [buyCount, sellCount, buyVolumeUsd,
// sellVolumeUsd, priceChangePercent]. A pair with no counted trades has
// windows null.
type statsBody struct {
	Windows [][]float64 `json:"windows"`
}

const (
	window5m = 0
	window1h = 1
)

func volumeFromBody(status int, body string) (domain.Volume, error) {
	if status != 200 {
		return domain.Volume{}, fmt.Errorf("axiom: HTTP %d", status)
	}
	var raw statsBody
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return domain.Volume{}, fmt.Errorf("axiom: decode: %w", err)
	}
	var v domain.Volume
	v.USD5m, v.Trades5m = window(raw.Windows, window5m)
	v.USD1h, v.Trades1h = window(raw.Windows, window1h)
	return v, nil
}

func window(ws [][]float64, i int) (usd float64, trades int) {
	if i >= len(ws) || len(ws[i]) < 4 {
		return 0, 0
	}
	w := ws[i]
	return w[2] + w[3], int(w[0] + w[1])
}
