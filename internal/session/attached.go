package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// pageSession is one Chrome DevTools attachment. A debug URL attaches to a
// Chrome the user already started and leaves that fomo tab open. A profile
// visit opens one tab in the bot's long-lived Chrome and closes only that tab.
type pageSession struct {
	opt      ChromeOptions
	log      *slog.Logger
	reuseTab bool

	mu          sync.Mutex
	started     bool
	allocCtx    context.Context
	allocCancel context.CancelFunc
	ctx         context.Context
	cancel      context.CancelFunc
	ownsTab     bool
	navigated   bool
}

func newPageSession(opt ChromeOptions, log *slog.Logger, reuseTab bool) *pageSession {
	if log == nil {
		log = slog.Default()
	}
	if opt.AppURL == "" {
		opt.AppURL = "https://fomo.family/"
	}
	if opt.LoadTimeout <= 0 {
		opt.LoadTimeout = 60 * time.Second
	}
	if opt.RequestTimeout <= 0 {
		opt.RequestTimeout = 15 * time.Second
	}
	return &pageSession{opt: opt, log: log, reuseTab: reuseTab}
}

// Start attaches to Chrome. The first chromedp.Run uses the long-lived session
// context: chromedp binds the target executor to that context, and a timeout
// on the first Run kills every later call with "context canceled".
func (s *pageSession) Start(parent context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return nil
	}
	if err := parent.Err(); err != nil {
		return err
	}

	debugURL := strings.TrimRight(strings.TrimSpace(s.opt.DebugURL), "/")
	var existing *debugTarget
	if s.reuseTab {
		if debugURL == "" {
			return errors.New("debug URL is empty")
		}
		var findErr error
		existing, findErr = findFomoTarget(parent, debugURL, s.opt.AppURL, 3*time.Second)
		if findErr != nil {
			s.log.Warn("remote target discovery failed", "account", s.opt.AccountID, "debug_url", debugURL, "err", findErr)
		}
		// Background, not the bot context: cancelling the bot must not close
		// a Chrome the user launched.
		s.allocCtx, s.allocCancel = chromedp.NewRemoteAllocator(context.Background(), debugURL)
	} else {
		remote, err := remoteAllocator(s.opt)
		if err != nil {
			return err
		}
		s.allocCtx = remote
		if url, ok := devtoolsURL(s.opt.ProfileDir); ok {
			debugURL = url
		}
	}

	if existing != nil {
		s.ctx, s.cancel = chromedp.NewContext(s.allocCtx, chromedp.WithTargetID(target.ID(existing.ID)))
		s.ownsTab = false
	} else {
		s.ctx, s.cancel = chromedp.NewContext(s.allocCtx)
		s.ownsTab = !s.reuseTab
	}

	if err := chromedp.Run(s.ctx); err != nil {
		s.failStart()
		return fmt.Errorf("initialize chrome target: %w", err)
	}

	startCtx, cancel := s.operationContext(parent, s.opt.LoadTimeout)
	defer cancel()

	var scriptID page.ScriptIdentifier
	actions := []chromedp.Action{
		chromedp.ActionFunc(func(ctx context.Context) error {
			id, err := page.AddScriptToEvaluateOnNewDocument(authCaptureScript).Do(ctx)
			if err == nil {
				scriptID = id
			}
			return err
		}),
		chromedp.Evaluate(authCaptureScript, nil),
	}
	if existing == nil {
		actions = append(actions, chromedp.Navigate(s.opt.AppURL))
	}
	if err := chromedp.Run(startCtx, actions...); err != nil {
		s.failStart()
		return fmt.Errorf("attach chrome: %w", err)
	}
	s.navigated = existing == nil
	if !s.reuseTab {
		HideHeadless(s.ctx)
	}

	var targetID target.ID
	var attachedURL string
	if existing != nil {
		targetID = target.ID(existing.ID)
		attachedURL = existing.URL
	}
	if c := chromedp.FromContext(s.ctx); c != nil && c.Target != nil && c.Target.TargetID != "" {
		targetID = c.Target.TargetID
	}
	if attachedURL == "" {
		_ = chromedp.Run(startCtx, chromedp.Location(&attachedURL))
	}

	s.started = true
	s.log.Info("browser_attached",
		"account", s.opt.AccountID,
		"debug_url", debugURL,
		"profile", s.opt.ProfileDir,
		"target_id", targetID,
		"url", attachedURL,
		"reused_existing_fomo_tab", existing != nil,
		"script_id", scriptID,
	)
	return nil
}

// closeOwnedTab closes a tab this visit created. A reused fomo tab and the
// Chrome process stay up: closing the DevTools context would close the page.
func (s *pageSession) closeOwnedTab() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ownsTab || s.cancel == nil {
		return
	}
	s.cancel()
	s.cancel = nil
	s.ctx = nil
	s.started = false
}

func (s *pageSession) failStart() {
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	if s.allocCancel != nil {
		s.allocCancel()
		s.allocCancel = nil
	}
	s.ctx = nil
	s.allocCtx = nil
	s.started = false
}

// Reload opens the app again so the page's own Privy SDK can refresh the session.
func (s *pageSession) Reload(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return errors.New("browser not attached")
	}
	c, cancel := s.operationContext(ctx, s.opt.LoadTimeout)
	defer cancel()
	return chromedp.Run(c, chromedp.Navigate(s.opt.AppURL))
}

func (s *pageSession) AccessToken(ctx context.Context) (string, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return "", time.Time{}, errors.New("browser not attached")
	}
	c, cancel := s.operationContext(ctx, s.opt.RequestTimeout)
	defer cancel()

	var tc tokenCandidate
	if err := chromedp.Run(c, chromedp.Evaluate(findTokenScript, &tc)); err != nil {
		return "", time.Time{}, err
	}
	if tc.Token == "" {
		return "", time.Time{}, fmt.Errorf("%w: no fomo access token in the Chrome tab", ErrNoToken)
	}
	tok := Normalize(tc.Token)
	exp, err := ParseExpiry(tok)
	if err != nil {
		return "", time.Time{}, err
	}
	s.log.Debug("access token observed", "account", s.opt.AccountID, "source", tc.Source, "expires", exp.Format(time.RFC3339))
	return tok, exp, nil
}

type tokenCandidate struct {
	Token  string `json:"token"`
	Exp    int64  `json:"exp"`
	Source string `json:"source"`
}

type pageFetchResult struct {
	HTTPStatus        int               `json:"httpStatus"`
	Body              string            `json:"body"`
	Headers           map[string]string `json:"headers"`
	FetchError        string            `json:"fetchError"`
	FetchStartedAt    string            `json:"fetchStartedAt"`
	HeadersReceivedAt string            `json:"headersReceivedAt"`
	BodyReadAt        string            `json:"bodyReadAt"`
}

// FetchInPage performs the HTTP call inside the fomo tab with cache: no-store.
func (s *pageSession) FetchInPage(ctx context.Context, method, rawURL string, hdr map[string]string, body []byte) (int, []byte, map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return 0, nil, nil, errors.New("browser not attached")
	}

	jsURL, _ := json.Marshal(rawURL)
	jsMethod, _ := json.Marshal(method)
	jsHdr, _ := json.Marshal(hdr)
	jsBody := "undefined"
	if len(body) > 0 {
		b, _ := json.Marshal(string(body))
		jsBody = string(b)
	}
	script := fmt.Sprintf(`(async () => {
      try {
        const headers = %s;
        const init = {
          method: %s,
          credentials: "include",
          cache: "no-store",
          headers
        };
        const body = %s;
        if (body !== undefined) init.body = body;
        const fetchStartedAt = new Date().toISOString();
        const r = await fetch(%s, init);
        const headersReceivedAt = new Date().toISOString();
        const text = await r.text();
        const bodyReadAt = new Date().toISOString();
        const headersOut = {};
        r.headers.forEach((v, k) => { headersOut[k] = v; });
        return {httpStatus: r.status, body: text, headers: headersOut, fetchError: "", fetchStartedAt, headersReceivedAt, bodyReadAt};
      } catch (e) {
        return {httpStatus: 0, body: "", headers: {}, fetchError: String(e && e.message || e)};
      }
    })()`, string(jsHdr), string(jsMethod), jsBody, string(jsURL))

	c, cancel := s.operationContext(ctx, s.opt.RequestTimeout)
	defer cancel()

	var out pageFetchResult
	awaitPromise := func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
		return p.WithAwaitPromise(true)
	}
	if err := chromedp.Run(c, chromedp.Evaluate(script, &out, awaitPromise)); err != nil {
		return 0, nil, nil, err
	}
	if out.FetchError != "" {
		return 0, nil, nil, fmt.Errorf("browser fetch: %s", out.FetchError)
	}
	if out.HTTPStatus == 0 {
		return 0, nil, nil, errors.New("browser fetch returned no HTTP status")
	}
	if out.Headers == nil {
		out.Headers = map[string]string{}
	}
	// Carried back to the fomo request log. Not sent to prod-api.
	if out.FetchStartedAt != "" {
		out.Headers["x-fomo-fetch-started-at"] = out.FetchStartedAt
	}
	if out.HeadersReceivedAt != "" {
		out.Headers["x-fomo-headers-received-at"] = out.HeadersReceivedAt
	}
	if out.BodyReadAt != "" {
		out.Headers["x-fomo-body-read-at"] = out.BodyReadAt
	}
	return out.HTTPStatus, []byte(out.Body), out.Headers, nil
}

func (s *pageSession) operationContext(caller context.Context, max time.Duration) (context.Context, context.CancelFunc) {
	// Keep chromedp's executor from s.ctx, and also stop when the caller is done.
	// A timeout derived from the caller drops the executor, because the caller
	// is not the context that first ran against this target.
	c, cancel := context.WithTimeout(s.ctx, max)
	stop := context.AfterFunc(caller, cancel)
	return c, func() {
		stop()
		cancel()
	}
}

type debugTarget struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

func findFomoTarget(ctx context.Context, debugURL, appURL string, timeout time.Duration) (*debugTarget, error) {
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, strings.TrimRight(debugURL, "/")+"/json/list", nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("debug endpoint returned HTTP %d", resp.StatusCode)
	}

	var targets []debugTarget
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return nil, err
	}

	appHost := "fomo.family"
	if u, err := url.Parse(appURL); err == nil && u.Host != "" {
		appHost = strings.ToLower(u.Host)
	}

	var fallback *debugTarget
	for i := range targets {
		t := &targets[i]
		if t.Type != "page" {
			continue
		}
		u, err := url.Parse(t.URL)
		if err != nil || strings.ToLower(u.Host) != appHost {
			continue
		}
		if t.URL == appURL || strings.HasPrefix(u.Path, "/token") {
			copy := *t
			return &copy, nil
		}
		if fallback == nil {
			copy := *t
			fallback = &copy
		}
	}
	return fallback, nil
}
