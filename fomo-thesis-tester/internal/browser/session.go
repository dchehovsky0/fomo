package browser

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

	"fomo-thesis-tester/internal/fomo"
)

type Options struct {
	AccountID      string
	ProfileDir     string
	DebugURL       string
	AppURL         string
	ReadyTimeout   time.Duration
	RequestTimeout time.Duration
	Log            *slog.Logger
}

type Session struct {
	opt         Options
	allocCtx    context.Context
	allocCancel context.CancelFunc
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	started     bool
	attachedURL string
	targetID    target.ID
}

type tokenCandidate struct {
	Token  string `json:"token"`
	Exp    int64  `json:"exp"`
	Source string `json:"source"`
}

type debugTarget struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

func New(opt Options) *Session { return &Session{opt: opt} }

// Start attaches to an already-running, normal Chrome instance exposed through
// --remote-debugging-port. It does not launch Chrome and does not touch the
// Chrome profile on disk. This lets the manually authenticated Fomo/Privy
// session remain the owner of auth state.
func (s *Session) Start(parent context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return nil
	}
	if strings.TrimSpace(s.opt.DebugURL) == "" {
		return errors.New("remote debug URL is empty")
	}

	debugURL := strings.TrimRight(strings.TrimSpace(s.opt.DebugURL), "/")

	// Prefer the Fomo tab the user already logged into. This avoids creating a
	// second Privy application page/session owner in the same Chrome profile.
	existing, findErr := findFomoTarget(parent, debugURL, s.opt.AppURL, 3*time.Second)
	if findErr != nil {
		s.opt.Log.Warn("remote_target_discovery_failed", "account", s.opt.AccountID, "debug_url", debugURL, "error", findErr)
	}

	// RemoteAllocator connects to the existing browser process; it never launches
	// a new Chrome. Keep this attachment independent from the application's root
	// context: cancelling our tester must not close a manually launched Chrome tab.
	// The OS will tear down the CDP websocket when this process exits.
	s.allocCtx, s.allocCancel = chromedp.NewRemoteAllocator(context.Background(), debugURL)
	if existing != nil {
		s.targetID = target.ID(existing.ID)
		s.attachedURL = existing.URL
		s.ctx, s.cancel = chromedp.NewContext(s.allocCtx, chromedp.WithTargetID(s.targetID))
	} else {
		// Fallback for a still-running Chrome with no Fomo tab: create exactly one
		// controlled tab in that same browser/profile. Existing login state is
		// reused, so this does not require another Google/Fomo login.
		s.ctx, s.cancel = chromedp.NewContext(s.allocCtx)
	}

	// IMPORTANT: the first chromedp.Run must use the long-lived session context.
	// chromedp binds the target executor to the context used by the first Run.
	// If that first Run uses a short-lived timeout context, cancelling the timeout
	// kills the target executor and every later action fails with "context canceled".
	if err := chromedp.Run(s.ctx); err != nil {
		s.closeOnStartError()
		return fmt.Errorf("initialize remote target: %w", err)
	}

	startCtx, cancel := context.WithTimeout(s.ctx, s.opt.ReadyTimeout)
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
		// AddScriptToEvaluateOnNewDocument only affects future document loads.
		// Install the hook in the already-open page too.
		chromedp.Evaluate(authCaptureScript, nil),
	}
	if existing == nil {
		actions = append(actions,
			chromedp.Navigate(s.opt.AppURL),
			chromedp.Sleep(2*time.Second),
		)
	}

	if err := chromedp.Run(startCtx, actions...); err != nil {
		s.closeOnStartError()
		return fmt.Errorf("attach remote chrome: %w", err)
	}

	if c := chromedp.FromContext(s.ctx); c != nil && c.Target != nil {
		s.targetID = c.Target.TargetID
	}
	if s.attachedURL == "" {
		var current string
		if err := chromedp.Run(startCtx, chromedp.Location(&current)); err == nil {
			s.attachedURL = current
		}
	}

	s.started = true
	s.opt.Log.Info("browser_attached",
		"account", s.opt.AccountID,
		"debug_url", debugURL,
		"profile", s.opt.ProfileDir,
		"target_id", s.targetID,
		"url", s.attachedURL,
		"reused_existing_fomo_tab", existing != nil,
		"script_id", scriptID,
	)
	return nil
}

// Close intentionally does not cancel a chromedp context attached to a manually
// launched Chrome target. chromedp closes an attached page when that context is
// cancelled. For this tester Chrome is user-owned, so process exit is allowed to
// drop the CDP websocket naturally while the tabs and profiles stay alive.
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = false
}

// closeOnStartError is only used before a session is handed to the scheduler.
// At that point cleanup is preferable to keeping a half-initialized attachment.
func (s *Session) closeOnStartError() {
	if s.cancel != nil {
		s.cancel()
	}
	if s.allocCancel != nil {
		s.allocCancel()
	}
	s.started = false
}

func (s *Session) ReloadAndRecover(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return errors.New("browser not started")
	}

	c, cancel := s.operationContext(ctx, s.opt.ReadyTimeout)
	defer cancel()

	if err := chromedp.Run(c,
		chromedp.Navigate(s.opt.AppURL),
		chromedp.Sleep(3*time.Second),
	); err != nil {
		return err
	}

	// The page owns Privy. We do not call the Privy refresh endpoint ourselves.
	// Reloading the real app page lets its own SDK hydrate/refresh the session.
	return nil
}

func (s *Session) AccessToken(ctx context.Context) (string, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return "", time.Time{}, errors.New("browser not started")
	}

	c, cancel := s.operationContext(ctx, s.opt.RequestTimeout)
	defer cancel()

	var raw json.RawMessage
	if err := chromedp.Run(c, chromedp.Evaluate(findTokenScript, &raw)); err != nil {
		return "", time.Time{}, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return "", time.Time{}, errors.New("no access token observed in Fomo page")
	}
	var tc tokenCandidate
	if err := json.Unmarshal(raw, &tc); err != nil {
		return "", time.Time{}, err
	}
	if tc.Token == "" || tc.Exp == 0 {
		return "", time.Time{}, errors.New("invalid access token candidate")
	}
	exp := time.Unix(tc.Exp, 0).UTC()
	s.opt.Log.Debug("access_token_observed", "account", s.opt.AccountID, "source", tc.Source, "expires_at", exp.Format(time.RFC3339))
	return tc.Token, exp, nil
}

func (s *Session) FetchTheses(ctx context.Context, tokenValue string, query fomo.ThesisQuery) (fomo.BrowserHTTPResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return fomo.BrowserHTTPResponse{}, errors.New("browser not started")
	}

	q := url.Values{}
	q.Set("tokenAddress", query.Address)
	q.Set("networkId", fmt.Sprintf("%d", query.NetworkID))
	q.Set("afterTime", fmt.Sprintf("%d", query.AfterTime.UnixMilli()))
	q.Set("beforeTime", fmt.Sprintf("%d", query.BeforeTime.UnixMilli()))
	q.Set("limit", fmt.Sprintf("%d", query.Limit))
	q.Set("threshold", fmt.Sprintf("%d", query.Threshold))
	endpoint := "https://prod-api.fomo.family/feed/token/sortedThesis?" + q.Encode()

	auth := ""
	if strings.TrimSpace(tokenValue) != "" {
		auth = "Bearer " + strings.TrimSpace(tokenValue)
	}
	jsEndpoint, _ := json.Marshal(endpoint)
	jsAuth, _ := json.Marshal(auth)

	script := fmt.Sprintf(`(async () => {
      try {
        const headers = {
          "Accept":"application/json",
          "X-Supported-Chains":"1,56,143,4663,8453,1399811149"
        };
        const auth = %s;
        if (auth) headers["Authorization"] = auth;
        const fetchStartedAt = new Date().toISOString();
        const r = await fetch(%s, {
          method: "GET",
          credentials: "include",
          cache: "no-store",
          headers
        });
        const headersReceivedAt = new Date().toISOString();
        const body = await r.text();
        const bodyReadAt = new Date().toISOString();
        return {
          httpStatus: r.status,
          body,
          retryAfter: r.headers.get("retry-after") || "",
          date: r.headers.get("date") || "",
          fetchStartedAt,
          headersReceivedAt,
          bodyReadAt
        };
      } catch (e) {
        return {httpStatus: 0, body: "", retryAfter: "", date: "", fetchError: String(e && e.message || e)};
      }
    })()`, string(jsAuth), string(jsEndpoint))

	c, cancel := s.operationContext(ctx, s.opt.RequestTimeout)
	defer cancel()

	var out struct {
		HTTPStatus        int    `json:"httpStatus"`
		Body              string `json:"body"`
		RetryAfter        string `json:"retryAfter"`
		Date              string `json:"date"`
		FetchStartedAt    string `json:"fetchStartedAt"`
		HeadersReceivedAt string `json:"headersReceivedAt"`
		BodyReadAt        string `json:"bodyReadAt"`
		FetchError        string `json:"fetchError"`
	}

	awaitPromise := func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
		return p.WithAwaitPromise(true)
	}
	if err := chromedp.Run(c, chromedp.Evaluate(script, &out, awaitPromise)); err != nil {
		return fomo.BrowserHTTPResponse{}, err
	}
	if out.FetchError != "" {
		return fomo.BrowserHTTPResponse{}, fmt.Errorf("browser fetch: %s", out.FetchError)
	}
	if out.HTTPStatus == 0 {
		return fomo.BrowserHTTPResponse{}, errors.New("browser fetch returned no HTTP status")
	}
	return fomo.BrowserHTTPResponse{
		HTTPStatus:        out.HTTPStatus,
		Body:              out.Body,
		RetryAfter:        out.RetryAfter,
		Date:              out.Date,
		FetchStartedAt:    out.FetchStartedAt,
		HeadersReceivedAt: out.HeadersReceivedAt,
		BodyReadAt:        out.BodyReadAt,
	}, nil
}

func (s *Session) operationContext(caller context.Context, max time.Duration) (context.Context, context.CancelFunc) {
	// Keep the chromedp context value/executor from s.ctx, but also propagate
	// cancellation from the caller. context.WithTimeout(caller, ...) cannot be used
	// because caller is a scheduler context and does not carry chromedp's executor.
	c, cancel := context.WithTimeout(s.ctx, max)
	stop := context.AfterFunc(caller, cancel)
	return c, func() {
		stop()
		cancel()
	}
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
