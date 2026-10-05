package watcher

import (
	"cmp"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"fomobot/internal/domain"
	"fomobot/internal/fomo"
)

// Board is the watch list as the operator's page shows it. Tier comes from
// the schedule and the time since the token entered tier 1, the same clock
// the checks use. Tokens already alerted or dropped are not here: they are
// no longer in the watch list.
type Board struct {
	AsOf     time.Time    `json:"as_of"`
	Need     int          `json:"need"`
	Watching int          `json:"watching"`
	Tiers    []BoardTier  `json:"tiers"`
	Tokens   []BoardToken `json:"tokens"`
}

// BoardTier is one schedule band and how many watched tokens are in it now.
type BoardTier struct {
	Tier    int   `json:"tier"`
	Tokens  int   `json:"tokens"`
	EveryMS int64 `json:"every_ms"`
	LastsMS int64 `json:"lasts_ms"`
}

// BoardToken is one token currently being asked about on fomo.
type BoardToken struct {
	Mint     string `json:"mint"`
	Symbol   string `json:"symbol"`
	Name     string `json:"name"`
	Dex      string `json:"dex,omitempty"`
	Pool     string `json:"pool,omitempty"`
	Tier     int    `json:"tier"`
	AgeMS    int64  `json:"age_ms"`
	LeftMS   int64  `json:"left_ms"`
	EveryMS  int64  `json:"every_ms"`
	Theses   int    `json:"theses"`
	Checked  bool   `json:"checked"`
	Checks   int    `json:"checks"`
	NextMS   int64  `json:"next_ms"`
	FailKind string `json:"fail_kind,omitempty"`
	FomoURL  string `json:"fomo_url,omitempty"`
	AxiomURL string `json:"axiom_url,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	// Market numbers. market_cap, volume_usd and liquidity_usd are the latest
	// DexScreener sample. entry_* is the sample that let the token in.
	MarketCap         float64   `json:"market_cap,omitempty"`
	VolumeUSD         float64   `json:"volume_usd,omitempty"`
	LiquidityUSD      float64   `json:"liquidity_usd,omitempty"`
	EntryMarketCap    float64   `json:"entry_market_cap,omitempty"`
	EntryVolumeUSD    float64   `json:"entry_volume_usd,omitempty"`
	EntryLiquidityUSD float64   `json:"entry_liquidity_usd,omitempty"`
	QuoteAt           time.Time `json:"quote_at,omitzero"`
	Deployer          string    `json:"deployer,omitempty"`
	LiquiditySOL      float64   `json:"liquidity_sol,omitempty"`
	Website           string    `json:"website,omitempty"`
	Twitter           string    `json:"twitter,omitempty"`
	Telegram          string    `json:"telegram,omitempty"`
	Discord           string    `json:"discord,omitempty"`
}

// seenThesis is one thesis from the last successful poll.
type seenThesis struct {
	at          time.Time
	text        string
	handle      string
	avatar      string
	verified    bool
	isDev       bool
	closed      bool
	positionUSD float64
}

// publishCount stores the latest thesis count where Board can read it.
// The check goroutine is the only writer; the page reads from another one.
func (w *Watcher) publishCount(it *item, count int) {
	w.mu.Lock()
	it.checks++
	it.count = count
	w.mu.Unlock()
}

// publishPage stores the count and the thesis texts from one poll.
// The open-token view reads this copy; it does not ask fomo again.
func (w *Watcher) publishPage(it *item, page *fomo.TokenThesisPage, at time.Time) {
	if page == nil {
		w.publishCount(it, 0)
		return
	}
	snap := make([]seenThesis, 0, len(page.Items))
	image := ""
	for _, t := range page.Items {
		if image == "" {
			image = domain.PictureURL(t.TokenImageURL)
		}
		snap = append(snap, seenThesis{
			at: atOr(t.CreatedAt, at), text: strings.Join(strings.Fields(t.Comment), " "),
			handle: t.Handle, avatar: t.AuthorImageURL, verified: t.Verified, isDev: t.IsDev,
			closed: t.PositionClosed, positionUSD: t.PositionUSD,
		})
	}
	w.mu.Lock()
	it.checks++
	it.count = page.Observed()
	it.seen = snap
	it.seenAt = at
	if image != "" {
		it.image = image
	}
	w.mu.Unlock()
}

func atOr(t, fallback time.Time) time.Time {
	if t.IsZero() {
		return fallback
	}
	return t
}

// clearMiss forgets request failures after an answer that came back.
func (w *Watcher) clearMiss(it *item) {
	w.mu.Lock()
	it.fails = 0
	it.failKind = ""
	it.failSpans = nil
	w.mu.Unlock()
}

// Board copies the watch list under the watcher lock.
func (w *Watcher) Board() Board {
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	b := Board{
		AsOf:     now,
		Need:     w.cfg.MinTheses,
		Watching: len(w.items),
		Tiers:    w.tierMeta(),
		Tokens:   make([]BoardToken, 0, len(w.items)),
	}
	for _, it := range w.items {
		age := now.Sub(it.addedAt)
		if age < 0 {
			age = 0
		}
		tier := w.tierNumber(age)
		every := w.interval(age)
		var left time.Duration
		if tier >= 1 && tier-1 < len(w.cfg.Schedule) {
			left = it.addedAt.Add(w.cfg.Schedule[tier-1].MaxAge).Sub(now)
			if left < 0 {
				left = 0
			}
		}
		var next int64
		if !it.due.IsZero() {
			next = it.due.Sub(now).Milliseconds()
		}
		b.Tokens = append(b.Tokens, BoardToken{
			Mint: it.l.Mint, Symbol: it.l.Symbol, Name: it.l.Name, Dex: it.l.Dex, Pool: it.l.Pool,
			Tier: tier, AgeMS: age.Milliseconds(), LeftMS: left.Milliseconds(), EveryMS: every.Milliseconds(),
			Theses: it.count, Checked: it.checks > 0, Checks: it.checks, NextMS: next, FailKind: it.failKind,
			ImageURL:  pickImage(it),
			MarketCap: it.marketCap, VolumeUSD: it.volumeUSD, LiquidityUSD: it.liquidityUSD,
			EntryMarketCap: it.l.EntryMarketCap, EntryVolumeUSD: it.l.EntryVolumeUSD, EntryLiquidityUSD: it.l.EntryLiquidityUSD,
			QuoteAt: it.quoteAt, Deployer: it.l.Deployer, LiquiditySOL: it.l.LiquiditySOL,
			Website: it.l.Website, Twitter: it.l.Twitter, Telegram: it.l.Telegram, Discord: it.l.Discord,
		})
		if tier >= 1 && tier <= len(b.Tiers) {
			b.Tiers[tier-1].Tokens++
		}
	}
	slices.SortFunc(b.Tokens, func(a, c BoardToken) int {
		if n := cmp.Compare(a.AgeMS, c.AgeMS); n != 0 {
			return n
		}
		return strings.Compare(a.Mint, c.Mint)
	})
	return b
}

// tierMeta describes each schedule band. LastsMS is how long the band itself
// runs, not the age at which it ends.
func (w *Watcher) tierMeta() []BoardTier {
	out := make([]BoardTier, len(w.cfg.Schedule))
	var prev time.Duration
	for i, t := range w.cfg.Schedule {
		lasts := t.MaxAge - prev
		if lasts < 0 {
			lasts = 0
		}
		out[i] = BoardTier{Tier: i + 1, EveryMS: t.Every.Milliseconds(), LastsMS: lasts.Milliseconds()}
		prev = t.MaxAge
	}
	return out
}

// BoardHandler serves Board as JSON. fomoTpl and axiomTpl use the same
// placeholders as the alert links, {mint} and {pair}. An empty template or a
// token without a pool leaves that URL out.
func (w *Watcher) BoardHandler(fomoTpl, axiomTpl string) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		b := w.Board()
		for i := range b.Tokens {
			b.Tokens[i].FomoURL = linkTemplate(fomoTpl, b.Tokens[i].Mint, b.Tokens[i].Pool)
			b.Tokens[i].AxiomURL = linkTemplate(axiomTpl, b.Tokens[i].Mint, b.Tokens[i].Pool)
		}
		body, err := json.Marshal(b)
		if err != nil {
			http.Error(rw, "board", http.StatusInternalServerError)
			return
		}
		h := rw.Header()
		h.Set("Content-Type", "application/json; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		rw.Write(body)
	})
}

// LiveToken is one watched token opened from the board. Theses are the last
// fomo answer. Market cap, volume and liquidity are the latest DexScreener
// sample, with the entry snapshot kept beside them.
type LiveToken struct {
	Mint              string       `json:"mint"`
	Symbol            string       `json:"symbol"`
	Name              string       `json:"name"`
	Chain             string       `json:"chain,omitempty"`
	Dex               string       `json:"dex,omitempty"`
	Tier              int          `json:"tier"`
	ImageURL          string       `json:"image_url,omitempty"`
	Count             int          `json:"count"`
	Need              int          `json:"need"`
	Checked           bool         `json:"checked"`
	CheckedAt         time.Time    `json:"checked_at,omitzero"`
	CreatedAt         time.Time    `json:"created_at,omitzero"`
	FomoURL           string       `json:"fomo_url,omitempty"`
	AxiomURL          string       `json:"axiom_url,omitempty"`
	Live              bool         `json:"live"`
	Theses            []LiveThesis `json:"theses"`
	MarketCap         float64      `json:"market_cap,omitempty"`
	VolumeUSD         float64      `json:"volume_usd,omitempty"`
	LiquidityUSD      float64      `json:"liquidity_usd,omitempty"`
	EntryMarketCap    float64      `json:"entry_market_cap,omitempty"`
	EntryVolumeUSD    float64      `json:"entry_volume_usd,omitempty"`
	EntryLiquidityUSD float64      `json:"entry_liquidity_usd,omitempty"`
	QuoteAt           time.Time    `json:"quote_at,omitzero"`
	Deployer          string       `json:"deployer,omitempty"`
	LiquiditySOL      float64      `json:"liquidity_sol,omitempty"`
	Website           string       `json:"website,omitempty"`
	Twitter           string       `json:"twitter,omitempty"`
	Telegram          string       `json:"telegram,omitempty"`
	Discord           string       `json:"discord,omitempty"`
}

// LiveThesis is one thesis from the last poll.
type LiveThesis struct {
	At          time.Time `json:"at"`
	Text        string    `json:"text"`
	Handle      string    `json:"handle,omitempty"`
	AvatarURL   string    `json:"avatar_url,omitempty"`
	Verified    bool      `json:"verified,omitempty"`
	IsDev       bool      `json:"is_dev,omitempty"`
	Closed      bool      `json:"closed,omitempty"`
	PositionUSD float64   `json:"position_usd"`
}

// Live copies the last poll of mint. ok is false when the token is not
// on the watch list.
func (w *Watcher) Live(mint, fomoTpl, axiomTpl string) (LiveToken, bool) {
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	it, ok := w.items[mint]
	if !ok || it.gone {
		return LiveToken{}, false
	}
	age := now.Sub(it.addedAt)
	if age < 0 {
		age = 0
	}
	theses := make([]LiveThesis, len(it.seen))
	for i, t := range it.seen {
		theses[i] = LiveThesis{
			At: t.at, Text: t.text, Handle: t.handle, AvatarURL: t.avatar,
			Verified: t.verified, IsDev: t.isDev, Closed: t.closed, PositionUSD: t.positionUSD,
		}
	}
	image := pickImage(it)
	return LiveToken{
		Mint: it.l.Mint, Symbol: it.l.Symbol, Name: it.l.Name, Chain: "Solana", Dex: it.l.Dex,
		Tier: w.tierNumber(age), ImageURL: image, Count: it.count, Need: w.cfg.MinTheses,
		Checked: it.checks > 0, CheckedAt: it.seenAt, CreatedAt: it.l.CreatedAt,
		FomoURL: linkTemplate(fomoTpl, it.l.Mint, ""), AxiomURL: linkTemplate(axiomTpl, it.l.Mint, it.l.Pool),
		Live: true, Theses: theses,
		MarketCap: it.marketCap, VolumeUSD: it.volumeUSD, LiquidityUSD: it.liquidityUSD,
		EntryMarketCap: it.l.EntryMarketCap, EntryVolumeUSD: it.l.EntryVolumeUSD, EntryLiquidityUSD: it.l.EntryLiquidityUSD,
		QuoteAt: it.quoteAt, Deployer: it.l.Deployer, LiquiditySOL: it.l.LiquiditySOL,
		Website: it.l.Website, Twitter: it.l.Twitter, Telegram: it.l.Telegram, Discord: it.l.Discord,
	}, true
}

// LiveHandler serves Live as JSON. A mint that is not being watched is 404.
func (w *Watcher) LiveHandler(fomoTpl, axiomTpl string) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		d, ok := w.Live(r.PathValue("mint"), fomoTpl, axiomTpl)
		if !ok {
			http.NotFound(rw, r)
			return
		}
		body, err := json.Marshal(d)
		if err != nil {
			http.Error(rw, "watch", http.StatusInternalServerError)
			return
		}
		h := rw.Header()
		h.Set("Content-Type", "application/json; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		rw.Write(body)
	})
}

func pickImage(it *item) string {
	if it.image != "" {
		return it.image
	}
	return it.l.ImageURL
}

// linkTemplate fills {mint} and {pair}. A template that needs a pool and
// a token without one produces no link.
func linkTemplate(tpl, mint, pool string) string {
	if tpl == "" || (strings.Contains(tpl, "{pair}") && pool == "") {
		return ""
	}
	return strings.NewReplacer("{mint}", mint, "{pair}", pool).Replace(tpl)
}
