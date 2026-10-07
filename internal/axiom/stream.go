package axiom

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"fomobot/internal/session"
)

const (
	frameBinding  = "__fomoAxiomFrame"
	statusBinding = "__fomoAxiomStatus"
	// seededFile in the profile remembers which config cookies were written
	// into it, so cookies the site refreshed later are not overwritten.
	seededFile = "fomo-seeded-cookies.json"
)

type Config struct {
	AppURL   string
	Clusters []string
	// Chrome: ProfileDir, ChromePath, Headless and NoSandbox are used.
	Chrome session.ChromeOptions
	// Cookies (auth-access-token, auth-refresh-token, cf_clearance) go into
	// the profile when it has none of that name or the value changed since it
	// was last written; otherwise the profile keeps the cookie the site
	// refreshed itself.
	Cookies map[string]string
	// StallTimeout: no new pair for this long restarts Chrome.
	StallTimeout time.Duration
	StartTimeout time.Duration
}

type Stats struct {
	Pairs    atomic.Int64
	Bad      atomic.Int64 // new_pairs messages that did not parse
	Dropped  atomic.Int64 // messages lost because the reader fell behind
	Connects atomic.Int64
	Restarts atomic.Int64
	// LastPair is the unix ms of the last pair.
	LastPair atomic.Int64
}

// Stream keeps a Chrome open on Axiom with its own WebSocket joined to
// new_pairs. The socket lives in the page so it carries the browser's
// cookies and TLS fingerprint; its messages reach Go through a CDP binding.
// Pair-stats reads stay out of this window: they run in StatsPage.
type Stream struct {
	cfg   Config
	log   *slog.Logger
	Stats Stats
}

func NewStream(cfg Config, log *slog.Logger) (*Stream, error) {
	dir, err := filepath.Abs(cfg.Chrome.ProfileDir)
	if err != nil {
		return nil, err
	}
	cfg.Chrome.ProfileDir = dir
	if len(cfg.Clusters) == 0 {
		return nil, errors.New("axiom: no websocket clusters")
	}
	if cfg.StallTimeout <= 0 {
		cfg.StallTimeout = 3 * time.Minute
	}
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = 90 * time.Second
	}
	return &Stream{cfg: cfg, log: log}, nil
}

// Run calls handle for every new pair until ctx is done, restarting Chrome
// with backoff when it fails or the stream goes silent. handle runs on the
// reading goroutine and must be quick.
func (s *Stream) Run(ctx context.Context, handle func(Pair)) {
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := s.session(ctx, handle)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > 5*time.Minute {
			backoff = 5 * time.Second
		}
		s.Stats.Restarts.Add(1)
		s.log.Warn("axiom stream stopped, restarting chrome", "err", err,
			"up_for", time.Since(start).Round(time.Second), "retry_in", backoff)
		if sleep(ctx, backoff) != nil {
			return
		}
		backoff = min(backoff*2, 2*time.Minute)
	}
}

func (s *Stream) session(ctx context.Context, handle func(Pair)) error {
	sessionStart := time.Now()
	browserCtx, closeTab, err := session.OpenTab(ctx, s.cfg.Chrome)
	if err != nil {
		return fmt.Errorf("start chrome: %w", err)
	}
	defer closeTab()

	frames := make(chan string, 4096)
	status := make(chan string, 64)
	chromedp.ListenTarget(browserCtx, func(ev any) {
		b, ok := ev.(*runtime.EventBindingCalled)
		if !ok {
			return
		}
		var ch chan string
		switch b.Name {
		case frameBinding:
			ch = frames
		case statusBinding:
			ch = status
		default:
			return
		}
		select {
		case ch <- b.Payload:
		default:
			s.Stats.Dropped.Add(1)
		}
	})

	// The first Run starts the browser and ties it to browserCtx, not to the
	// start timeout below.
	if err := chromedp.Run(browserCtx); err != nil {
		return fmt.Errorf("start chrome: %w", err)
	}
	startCtx, cancelStart := context.WithTimeout(browserCtx, s.cfg.StartTimeout)
	err = s.open(startCtx)
	cancelStart()
	if err != nil {
		return err
	}
	s.log.Info("axiom page opened, waiting for new pairs", "url", s.cfg.AppURL,
		"took", time.Since(sessionStart).Round(time.Millisecond))

	started := time.Now()
	check := time.NewTicker(10 * time.Second)
	defer check.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-browserCtx.Done():
			return errors.New("chrome closed")
		case raw := <-frames:
			arrived := time.Now()
			p, ok := Parse([]byte(raw))
			if !ok {
				s.Stats.Bad.Add(1)
				s.log.Warn("unparsed new_pairs message", "took", time.Since(arrived).Round(time.Millisecond), "raw", raw[:min(len(raw), 500)])
				continue
			}
			s.Stats.Pairs.Add(1)
			s.Stats.LastPair.Store(arrived.UnixMilli())
			age := time.Duration(0)
			if !p.CreatedAt.IsZero() {
				age = arrived.Sub(p.CreatedAt).Round(time.Millisecond)
			}
			s.log.Info("flow", "step", "увидели", "where", "axiom",
				"token", p.Token, "ticker", p.Ticker, "protocol", p.Label(),
				"age", age, "parse", time.Since(arrived).Round(time.Millisecond))
			handle(p)
		case st := <-status:
			s.logStatus(st)
		case <-check.C:
			last := started
			if at := time.UnixMilli(s.Stats.LastPair.Load()); at.After(last) {
				last = at
			}
			if silent := time.Since(last); silent > s.cfg.StallTimeout {
				return fmt.Errorf("no new pairs for %s", silent.Round(time.Second))
			}
		}
	}
}

// open installs the socket script for every document of the tab, so a
// Cloudflare check or a reload by the site brings the socket back, and
// opens the app.
func (s *Stream) open(ctx context.Context) error {
	session.HideHeadless(ctx)
	return chromedp.Run(ctx,
		runtime.AddBinding(frameBinding),
		runtime.AddBinding(statusBinding),
		chromedp.ActionFunc(s.seedCookies),
		chromedp.ActionFunc(func(ctx context.Context) error {
			_, err := page.AddScriptToEvaluateOnNewDocument(s.script()).Do(ctx)
			return err
		}),
		chromedp.Navigate(s.cfg.AppURL),
	)
}

func (s *Stream) sincePair() time.Duration {
	last := s.Stats.LastPair.Load()
	if last == 0 {
		return 0
	}
	return time.Since(time.UnixMilli(last)).Round(time.Second)
}

func (s *Stream) logStatus(raw string) {
	var st struct {
		Event  string `json:"event"`
		URL    string `json:"url"`
		Code   int    `json:"code"`
		Reason string `json:"reason"`
	}
	if json.Unmarshal([]byte(raw), &st) != nil {
		return
	}
	switch st.Event {
	case "open":
		s.Stats.Connects.Add(1)
		s.log.Info("axiom websocket connected", "url", st.URL)
	case "close":
		s.log.Warn("axiom websocket closed, reconnecting", "url", st.URL, "code", st.Code, "reason", st.Reason,
			"since_pair", s.sincePair())
	}
}

// socketScript runs at the start of every top-level document. Before each
// connect it refreshes the access token cookie and makes the requests the
// site makes before its own socket.
const socketScript = `(() => {
  if (window.top !== window || !location.protocol.startsWith("http") || window.__fomoAxiom) return;
  window.__fomoAxiom = true;
  const clusters = __CLUSTERS__, room = __ROOM__, marker = JSON.stringify(room);
  const frame = (s) => window.__FRAME__(s);
  const status = (o) => window.__STATUS__(JSON.stringify(o));
  let next = 0, delay = 1000;
  const warmup = () => {
    const v = Date.now(), opts = () => ({credentials: "include", signal: AbortSignal.timeout(10000)});
    return Promise.allSettled([
      fetch("https://api.axiom.trade/refresh-access-token", {method: "POST", ...opts()}),
      fetch("https://api.axiom.trade/wo/server-time?v=" + v, opts()),
      fetch("https://api6.axiom.trade/get-announcement?v=" + v, opts()),
    ]);
  };
  const connect = async () => {
    await warmup();
    const url = clusters[next++ % clusters.length];
    const ws = new WebSocket(url);
    ws.onopen = () => {
      delay = 1000;
      ws.send(JSON.stringify({action: "join", room}));
      status({event: "open", url});
    };
    ws.onmessage = (e) => {
      if (typeof e.data === "string" && e.data.includes(marker)) frame(e.data);
    };
    ws.onclose = (e) => {
      status({event: "close", url, code: e.code, reason: e.reason});
      setTimeout(connect, delay);
      delay = Math.min(delay * 2, 30000);
    };
  };
  setTimeout(connect, 2000);
})();`

func (s *Stream) script() string {
	clusters, _ := json.Marshal(s.cfg.Clusters)
	room, _ := json.Marshal(Room)
	return strings.NewReplacer(
		"__CLUSTERS__", string(clusters), "__ROOM__", string(room),
		"__FRAME__", frameBinding, "__STATUS__", statusBinding,
	).Replace(socketScript)
}

func (s *Stream) seedCookies(ctx context.Context) error {
	if len(s.cfg.Cookies) == 0 {
		return nil
	}
	u, err := url.Parse(s.cfg.AppURL)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("axiom app url %q: %v", s.cfg.AppURL, err)
	}
	domain := "." + strings.TrimPrefix(u.Hostname(), "www.")

	path := filepath.Join(s.cfg.Chrome.ProfileDir, seededFile)
	seeded := map[string]string{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &seeded)
	}
	have, err := network.GetCookies().WithURLs([]string{s.cfg.AppURL}).Do(ctx)
	if err != nil {
		return fmt.Errorf("read axiom cookies: %w", err)
	}
	present := map[string]bool{}
	for _, c := range have {
		present[c.Name] = true
	}

	changed := false
	for name, val := range s.cfg.Cookies {
		if val == "" {
			continue
		}
		sum := sha256.Sum256([]byte(val))
		digest := hex.EncodeToString(sum[:])
		prev, known := seeded[name]
		if present[name] && (prev == digest || !known) {
			if !known {
				// The profile got its own session (-axiom-login) before this
				// value was ever written: keep the profile's cookie.
				seeded[name] = digest
				changed = true
			}
			continue
		}
		if err := network.SetCookie(name, val).WithDomain(domain).WithPath("/").WithSecure(true).Do(ctx); err != nil {
			return fmt.Errorf("set axiom cookie %s: %w", name, err)
		}
		seeded[name] = digest
		changed = true
		s.log.Info("axiom cookie written from config", "cookie", name)
	}
	if !changed {
		return nil
	}
	data, _ := json.Marshal(seeded)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		s.log.Warn("remember seeded axiom cookies", "err", err)
	}
	return nil
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
