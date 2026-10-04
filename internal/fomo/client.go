package fomo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"fomobot/internal/session"
)

var ErrUnauthorized = errors.New("fomo: access token rejected")

type HTTPError struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("fomo %s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Body)
}

type Options struct {
	BaseURL         string
	AppURL          string
	SupportedChains string
	NetworkID       string
	UserAgent       string
	Timeout         time.Duration
	RPS             float64
	MaxRetries      int
	ThesisThreshold string
	// Proxy is an optional http://, https:// or socks5:// proxy URL for all
	// requests of this client.
	Proxy string
	// OnCall, if set, is called after every HTTP attempt. It must not block.
	OnCall func(CallInfo)
	// Page, when set, performs the call inside the account's Chrome tab
	// instead of the Go HTTP client. The account proxy is not applied.
	Page PageTransport
}

// PageTransport is the attached Chrome tab from session.Chrome.
type PageTransport interface {
	FetchInPage(ctx context.Context, method, rawURL string, hdr map[string]string, body []byte) (status int, resp []byte, respHdr map[string]string, err error)
}

// CallInfo describes one HTTP attempt.
type CallInfo struct {
	At       time.Time
	Method   string
	Path     string
	Query    string
	Status   int // 0 when the request failed before a response
	Duration time.Duration
	Bytes    int
	Err      error
}

type Stats struct {
	Requests    atomic.Int64
	RateLimited atomic.Int64
	AuthErrors  atomic.Int64
	// HTTPNanos / HTTPSamples are the time spent inside the HTTP call itself,
	// without waiting for the rate limit or a fresh token.
	HTTPNanos   atomic.Int64
	HTTPSamples atomic.Int64
}

type Client struct {
	opts    Options
	origin  string
	httpc   *http.Client
	tokens  session.TokenSource
	page    PageTransport
	limiter *rate.Limiter
	log     *slog.Logger
	Stats   Stats

	mu             sync.Mutex
	blockedUntil   time.Time
	consecutive429 int
	rateLimitBase  time.Duration
	etags          map[string]cachedResponse
}

type cachedResponse struct {
	etag string
	body []byte
}

func NewClient(opts Options, tokens session.TokenSource, log *slog.Logger) (*Client, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	if opts.RPS <= 0 {
		opts.RPS = 1
	}
	if opts.NetworkID == "" {
		opts.NetworkID = SolanaNetworkID
	}
	origin := Origin(opts.AppURL)
	httpc := &http.Client{Timeout: opts.Timeout}
	if opts.Proxy != "" {
		pu, err := url.Parse(opts.Proxy)
		if err != nil || pu.Host == "" {
			return nil, fmt.Errorf("invalid proxy %q", redactProxy(opts.Proxy))
		}
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = http.ProxyURL(pu)
		httpc.Transport = tr
	}
	return &Client{
		opts:          opts,
		origin:        origin,
		httpc:         httpc,
		tokens:        tokens,
		page:          opts.Page,
		limiter:       rate.NewLimiter(rate.Limit(opts.RPS), 1),
		log:           log,
		rateLimitBase: 10 * time.Second,
		etags:         make(map[string]cachedResponse),
	}, nil
}

// redactProxy hides the password of a proxy URL for logs and errors.
func redactProxy(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	return u.Redacted()
}

func (c *Client) NetworkID() string { return c.opts.NetworkID }

// Origin is the site a fomo app URL belongs to, used as the WebSocket Origin.
func Origin(appURL string) string {
	if u, err := url.Parse(appURL); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return "https://fomo.family"
}

// Feed returns the latest thesis_created events from the global feed.
func (c *Client) Feed(ctx context.Context, limit int) ([]Event, error) {
	events, _, err := c.FeedWithStatus(ctx, limit)
	return events, err
}

// FeedWithStatus is Feed that also reports whether the server answered 304,
// i.e. the feed has not changed since the previous call.
func (c *Client) FeedWithStatus(ctx context.Context, limit int) ([]Event, bool, error) {
	q := url.Values{
		"limit":     {strconv.Itoa(limit)},
		"feedTypes": {TypeThesisCreated},
	}
	body, notModified, err := c.send(ctx, http.MethodGet, "/feed", q, nil, true)
	if err != nil {
		return nil, false, err
	}
	events, errs, err := ParseFeed(body)
	if err != nil {
		return nil, false, err
	}
	for _, e := range errs {
		c.log.Warn("skipped undecodable feed event", "err", e)
	}
	return events, notModified, nil
}

// TokenDetails returns the raw responseObject of /proxy/tokenDetails.
func (c *Client) TokenDetails(ctx context.Context, mint string) (json.RawMessage, error) {
	body, err := c.do(ctx, http.MethodPost, "/proxy/tokenDetails", nil,
		map[string]string{"tokenId": mint + ":" + c.opts.NetworkID}, false)
	if err != nil {
		return nil, err
	}
	return responseObject(body)
}

// FilterTokens returns the raw responseObject of /proxy/filterTokens for a
// batch of tokens.
func (c *Client) FilterTokens(ctx context.Context, mints []string) (json.RawMessage, error) {
	ids := make([]string, len(mints))
	for i, m := range mints {
		ids[i] = m + ":" + c.opts.NetworkID
	}
	body, err := c.do(ctx, http.MethodPost, "/proxy/filterTokens", nil, ids, false)
	if err != nil {
		return nil, err
	}
	return responseObject(body)
}

// TokenThesis returns one page of theses for a token. lastID is the id of the
// last item of the previous page ("" for the first page).
func (c *Client) TokenThesis(ctx context.Context, mint, lastID string) (*TokenThesisPage, error) {
	q := url.Values{
		"tokenAddress": {mint},
		"networkId":    {c.opts.NetworkID},
	}
	if c.opts.ThesisThreshold != "" {
		q.Set("threshold", c.opts.ThesisThreshold)
	}
	if lastID != "" {
		q.Set("lastId", lastID)
	}
	body, err := c.do(ctx, http.MethodGet, "/feed/token/thesis", q, nil, false)
	if err != nil {
		return nil, err
	}
	page, errs, err := ParseTokenThesis(body)
	if err != nil {
		return nil, err
	}
	for _, e := range errs {
		c.log.Warn("skipped undecodable token thesis", "token", mint, "err", e)
	}
	return page, nil
}

// SortedThesis returns up to limit theses of a token created in [after, before].
// Count is the token's total and is not limited by the time window, but it
// lags the items array. Use TokenThesisPage.Observed for the number to act on.
func (c *Client) SortedThesis(ctx context.Context, mint string, after, before time.Time, limit int) (*TokenThesisPage, error) {
	q := url.Values{
		"tokenAddress": {mint},
		"networkId":    {c.opts.NetworkID},
		"afterTime":    {strconv.FormatInt(after.UnixMilli(), 10)},
		"beforeTime":   {strconv.FormatInt(before.UnixMilli(), 10)},
		"limit":        {strconv.Itoa(limit)},
		"threshold":    {"0"},
	}
	body, err := c.do(ctx, http.MethodGet, "/feed/token/sortedThesis", q, nil, false)
	if err != nil {
		return nil, err
	}
	page, errs, err := ParseTokenThesis(body)
	if err != nil {
		return nil, err
	}
	for _, e := range errs {
		c.log.Warn("skipped undecodable token thesis", "token", mint, "err", e)
	}
	return page, nil
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, reqBody any, useETag bool) ([]byte, error) {
	body, _, err := c.send(ctx, method, path, q, reqBody, useETag)
	return body, err
}

func (c *Client) observe(start time.Time, method, path string, q url.Values, status, n int, err error) {
	if c.opts.OnCall == nil {
		return
	}
	c.opts.OnCall(CallInfo{
		At: start, Method: method, Path: path, Query: q.Encode(),
		Status: status, Duration: time.Since(start), Bytes: n, Err: err,
	})
}

func (c *Client) send(ctx context.Context, method, path string, q url.Values, reqBody any, useETag bool) ([]byte, bool, error) {
	u := c.opts.BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var payload []byte
	if reqBody != nil {
		var err error
		if payload, err = json.Marshal(reqBody); err != nil {
			return nil, false, err
		}
	}

	authRetried := false
	var lastErr error
	for attempt := 0; attempt <= c.opts.MaxRetries; attempt++ {
		backoffStart := time.Now()
		if err := c.waitRateLimitBackoff(ctx); err != nil {
			return nil, false, err
		}
		backoffWait := time.Since(backoffStart)
		limitStart := time.Now()
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, false, err
		}
		limitWait := time.Since(limitStart)
		tokenStart := time.Now()
		tok, err := c.tokens.Token(ctx)
		tokenWait := time.Since(tokenStart)
		if err != nil {
			c.log.Warn("fomo request skipped, no token",
				"path", path, "token", q.Get("tokenAddress"), "attempt", attempt+1,
				"token_wait", tokenWait.Round(time.Millisecond),
				"limit_wait", limitWait.Round(time.Millisecond), "err", err)
			return nil, false, fmt.Errorf("get access token: %w", err)
		}

		req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(payload))
		if err != nil {
			return nil, false, err
		}
		c.setHeaders(req, tok, payload != nil)
		cached, hasCached := c.cached(u)
		if useETag && hasCached {
			req.Header.Set("If-None-Match", cached.etag)
		}

		c.Stats.Requests.Add(1)
		start := time.Now()
		var resp *http.Response
		if c.page != nil {
			resp, err = c.doPage(ctx, req)
		} else {
			resp, err = c.httpc.Do(req)
		}
		httpTook := time.Since(start)
		c.noteHTTP(httpTook)
		if err != nil {
			if ctx.Err() != nil {
				return nil, false, ctx.Err()
			}
			c.observe(start, method, path, q, 0, 0, err)
			lastErr = err
			c.logRequest(method, path, q, 0, 0, attempt+1, httpTook, tokenWait, limitWait, backoffWait, nil, nil, err)
			if err := sleep(ctx, serverBackoff(attempt)); err != nil {
				return nil, false, err
			}
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		c.observe(start, method, path, q, resp.StatusCode, len(body), err)
		if err != nil {
			lastErr = err
			c.logRequest(method, path, q, statusOr(resp), 0, attempt+1, httpTook, tokenWait, limitWait, backoffWait, nil, resp.Header, err)
			if err := sleep(ctx, serverBackoff(attempt)); err != nil {
				return nil, false, err
			}
			continue
		}

		status := resp.StatusCode
		switch {
		case status == http.StatusNotModified && hasCached:
			c.resetRateLimitBackoff()
			c.logRequest(method, path, q, status, len(cached.body), attempt+1, httpTook, tokenWait, limitWait, backoffWait, cached.body, resp.Header, nil)
			return cached.body, true, nil

		case status >= 200 && status < 300:
			c.resetRateLimitBackoff()
			if etag := resp.Header.Get("ETag"); useETag && etag != "" {
				c.storeETag(u, etag, body)
			}
			c.logRequest(method, path, q, status, len(body), attempt+1, httpTook, tokenWait, limitWait, backoffWait, body, resp.Header, nil)
			return body, false, nil

		case status == http.StatusUnauthorized || status == 430 || status == 431:
			c.Stats.AuthErrors.Add(1)
			if authRetried {
				return nil, false, fmt.Errorf("%w: HTTP %d on %s", ErrUnauthorized, status, path)
			}
			authRetried = true
			c.log.Warn("fomo rejected token, refreshing", "status", status, "path", path,
				"token", q.Get("tokenAddress"), "http", httpTook.Round(time.Millisecond), "token_wait", tokenWait.Round(time.Millisecond))
			c.tokens.Invalidate()
			attempt--

		case status == http.StatusTooManyRequests:
			c.Stats.RateLimited.Add(1)
			delay := c.registerRateLimit(resp.Header.Get("Retry-After"))
			c.log.Warn("fomo rate limited (429)", "path", path, "token", q.Get("tokenAddress"),
				"http", httpTook.Round(time.Millisecond), "backoff", delay.Round(time.Millisecond))
			lastErr = &HTTPError{Method: method, Path: path, Status: status, Body: snippet(body)}

		case status >= 500:
			lastErr = &HTTPError{Method: method, Path: path, Status: status, Body: snippet(body)}
			c.log.Warn("fomo server error", "path", path, "token", q.Get("tokenAddress"),
				"status", status, "attempt", attempt+1, "http", httpTook.Round(time.Millisecond))
			if err := sleep(ctx, serverBackoff(attempt)); err != nil {
				return nil, false, err
			}

		default:
			err := &HTTPError{Method: method, Path: path, Status: status, Body: snippet(body)}
			c.logRequest(method, path, q, status, len(body), attempt+1, httpTook, tokenWait, limitWait, backoffWait, body, resp.Header, err)
			return nil, false, err
		}
	}
	return nil, false, fmt.Errorf("fomo %s %s: retries exhausted: %w", method, path, lastErr)
}

func statusOr(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

func (c *Client) noteHTTP(d time.Duration) {
	c.Stats.HTTPNanos.Add(int64(d))
	c.Stats.HTTPSamples.Add(1)
}

func (c *Client) logRequest(method, path string, q url.Values, status, n, attempt int, httpTook, tokenWait, limitWait, backoffWait time.Duration, body []byte, hdr http.Header, err error) {
	via := "http"
	if c.page != nil {
		via = "chrome"
	}
	args := []any{
		"via", via,
		"method", method, "path", path, "status", status, "bytes", n, "attempt", attempt,
		"http", httpTook.Round(time.Millisecond),
		"token_wait", tokenWait.Round(time.Millisecond),
		"limit_wait", limitWait.Round(time.Millisecond),
		"backoff_wait", backoffWait.Round(time.Millisecond),
	}
	if mint := q.Get("tokenAddress"); mint != "" {
		args = append(args, "token", mint)
	}
	if v := q.Get("networkId"); v != "" {
		args = append(args, "network_id", v)
	}
	if v := q.Get("afterTime"); v != "" {
		args = append(args, "after_ms", v, "before_ms", q.Get("beforeTime"))
	}
	if v := q.Get("limit"); v != "" {
		args = append(args, "limit", v)
	}
	if v := q.Get("threshold"); v != "" {
		args = append(args, "threshold", v)
	}
	if items, count, hasNext, bodyStatus, limitHit, ok := responseFacts(body, q.Get("limit")); ok {
		args = append(args,
			"body_status", bodyStatus,
			"items", items,
			"count", count,
			"has_next_page", hasNext,
			"limit_hit", limitHit,
			"window_complete", !hasNext && !limitHit,
		)
	}
	args = append(args, browserTiming(hdr)...)
	if err != nil {
		c.log.Warn("fomo request", append(args, "err", err)...)
		return
	}
	c.log.Info("fomo request", args...)
}

// responseFacts reads the thesis envelope. Error bodies use responseObject as
// an array, and those are left to the HTTP status alone.
func responseFacts(body []byte, limit string) (items, count int, hasNext bool, bodyStatus int, limitHit, ok bool) {
	if len(bytes.TrimSpace(body)) == 0 {
		return 0, 0, false, 0, false, false
	}
	var env struct {
		StatusCode     int             `json:"statusCode"`
		ResponseObject json.RawMessage `json:"responseObject"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return 0, 0, false, 0, false, false
	}
	raw := bytes.TrimSpace(env.ResponseObject)
	if len(raw) == 0 || raw[0] != '{' {
		if env.StatusCode != 0 {
			return 0, 0, false, env.StatusCode, false, true
		}
		return 0, 0, false, 0, false, false
	}
	var obj struct {
		Items       []json.RawMessage `json:"items"`
		HasNextPage bool              `json:"hasNextPage"`
		Count       int               `json:"count"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return 0, 0, false, env.StatusCode, false, env.StatusCode != 0
	}
	lim, _ := strconv.Atoi(limit)
	hit := lim > 0 && len(obj.Items) >= lim
	return len(obj.Items), obj.Count, obj.HasNextPage, env.StatusCode, hit, true
}

func browserTiming(hdr http.Header) []any {
	if hdr == nil {
		return nil
	}
	started := hdr.Get("X-Fomo-Fetch-Started-At")
	headersAt := hdr.Get("X-Fomo-Headers-Received-At")
	bodyAt := hdr.Get("X-Fomo-Body-Read-At")
	if started == "" && headersAt == "" && bodyAt == "" {
		return nil
	}
	args := []any{
		"browser_fetch_started_at", started,
		"browser_headers_received_at", headersAt,
		"browser_body_read_at", bodyAt,
	}
	if ms, ok := msBetween(started, headersAt); ok {
		args = append(args, "network_to_headers_ms", ms)
	}
	if ms, ok := msBetween(headersAt, bodyAt); ok {
		args = append(args, "body_read_ms", ms)
	}
	return args
}

func msBetween(a, b string) (int64, bool) {
	ta, ea := time.Parse(time.RFC3339Nano, a)
	tb, eb := time.Parse(time.RFC3339Nano, b)
	if ea != nil || eb != nil {
		return 0, false
	}
	return tb.Sub(ta).Milliseconds(), true
}

// doPage runs the request in the attached Chrome tab. Origin, Referer and
// User-Agent are left to the page: fetch refuses to set those headers.
func (c *Client) doPage(ctx context.Context, req *http.Request) (*http.Response, error) {
	hdr := map[string]string{}
	for _, k := range []string{"Authorization", "Accept", "Content-Type", "X-Supported-Chains", "If-None-Match"} {
		if v := req.Header.Get(k); v != "" {
			hdr[k] = v
		}
	}
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()
	status, respBody, respHdr, err := c.page.FetchInPage(callCtx, req.Method, req.URL.String(), hdr, body)
	if err != nil {
		return nil, err
	}
	h := make(http.Header, len(respHdr))
	for k, v := range respHdr {
		h.Set(k, v)
	}
	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       io.NopCloser(bytes.NewReader(respBody)),
		Request:    req,
	}, nil
}

func (c *Client) setHeaders(req *http.Request, token string, hasBody bool) {
	h := req.Header
	h.Set("Authorization", "Bearer "+token)
	h.Set("x-supported-chains", c.opts.SupportedChains)
	h.Set("Accept", "application/json")
	h.Set("Origin", c.origin)
	h.Set("Referer", c.origin+"/")
	if c.opts.UserAgent != "" {
		h.Set("User-Agent", c.opts.UserAgent)
	}
	if hasBody {
		h.Set("Content-Type", "application/json")
	}
}

// The 429 backoff is shared by all callers: once the API throttles us, every
// request waits, not only the one that got the 429.
func (c *Client) waitRateLimitBackoff(ctx context.Context) error {
	c.mu.Lock()
	wait := time.Until(c.blockedUntil)
	c.mu.Unlock()
	if wait <= 0 {
		return nil
	}
	return sleep(ctx, wait)
}

func (c *Client) registerRateLimit(retryAfter string) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.consecutive429++
	delay := c.rateLimitBase << min(c.consecutive429-1, 5)
	delay = min(delay, 30*c.rateLimitBase)
	if ra := parseRetryAfter(retryAfter); ra > delay {
		delay = ra
	}
	delay += time.Duration(rand.Int64N(int64(delay/5) + 1))
	c.blockedUntil = time.Now().Add(delay)
	return delay
}

func (c *Client) resetRateLimitBackoff() {
	c.mu.Lock()
	c.consecutive429 = 0
	c.mu.Unlock()
}

func (c *Client) cached(u string) (cachedResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.etags[u]
	return r, ok
}

func (c *Client) storeETag(u, etag string, body []byte) {
	c.mu.Lock()
	c.etags[u] = cachedResponse{etag: etag, body: body}
	c.mu.Unlock()
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return time.Until(t)
	}
	return 0
}

func serverBackoff(attempt int) time.Duration {
	d := time.Second << min(attempt, 5)
	d = min(d, 30*time.Second)
	return d + time.Duration(rand.Int64N(int64(d/4)+1))
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func snippet(b []byte) string {
	const max = 300
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}
