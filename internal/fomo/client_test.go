package fomo

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeTokens struct {
	token       atomic.Value
	invalidated atomic.Int32
}

func (f *fakeTokens) Token(context.Context) (string, error) { return f.token.Load().(string), nil }
func (f *fakeTokens) Invalidate() {
	f.invalidated.Add(1)
	f.token.Store("fresh")
}

func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *fakeTokens) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	tokens := &fakeTokens{}
	tokens.token.Store("stale")
	c, err := NewClient(Options{
		BaseURL: srv.URL, AppURL: "https://fomo.family/", SupportedChains: "1,1399811149",
		UserAgent: "test-ua", RPS: 1000, MaxRetries: 3,
	}, tokens, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	c.rateLimitBase = 10 * time.Millisecond
	return c, tokens
}

func TestClientHeadersAndAuthRetry(t *testing.T) {
	fixture, err := os.ReadFile("testdata/feed_thesis_response.json")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	c, tokens := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer fresh" {
			w.WriteHeader(431)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		for h, want := range map[string]string{
			"x-supported-chains": "1,1399811149", "Origin": "https://fomo.family",
			"Referer": "https://fomo.family/", "User-Agent": "test-ua", "Accept": "application/json",
		} {
			if got := r.Header.Get(h); got != want {
				t.Errorf("header %s = %q, want %q", h, got, want)
			}
		}
		if r.URL.Path != "/feed" || r.URL.Query().Get("feedTypes") != "thesis_created" || r.URL.Query().Get("limit") != "50" {
			t.Errorf("unexpected request %s", r.URL)
		}
		w.Write(fixture)
	})

	events, err := c.Feed(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 8 || tokens.invalidated.Load() != 1 || calls.Load() != 2 {
		t.Fatalf("events=%d invalidated=%d calls=%d", len(events), tokens.invalidated.Load(), calls.Load())
	}
}

func TestClientTokenThesisQuery(t *testing.T) {
	fixture, err := os.ReadFile("testdata/token_thesis_response.json")
	if err != nil {
		t.Fatal(err)
	}
	c, tokens := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/feed/token/thesis" || q.Get("tokenAddress") != "MINT" || q.Get("networkId") != SolanaNetworkID ||
			q.Get("threshold") != "0" || q.Get("lastId") != "t2-last" {
			t.Errorf("unexpected request %s", r.URL)
		}
		w.Write(fixture)
	})
	tokens.token.Store("fresh")
	c.opts.ThesisThreshold = "0"
	page, err := c.TokenThesis(context.Background(), "MINT", "t2-last")
	if err != nil {
		t.Fatal(err)
	}
	if page.Count != 663 || len(page.Items) != 25 {
		t.Fatalf("count=%d items=%d", page.Count, len(page.Items))
	}
}

func TestClientSortedThesisQuery(t *testing.T) {
	fixture, err := os.ReadFile("testdata/token_thesis_response.json")
	if err != nil {
		t.Fatal(err)
	}
	after := time.UnixMilli(1790582400000)
	before := time.UnixMilli(1790594981000)
	c, tokens := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/feed/token/sortedThesis" || q.Get("tokenAddress") != "MINT" || q.Get("networkId") != SolanaNetworkID ||
			q.Get("threshold") != "0" || q.Get("afterTime") != "1790582400000" || q.Get("beforeTime") != "1790594981000" ||
			q.Get("limit") != "500" {
			t.Errorf("unexpected request %s", r.URL)
		}
		w.Write(fixture)
	})
	tokens.token.Store("fresh")
	page, err := c.SortedThesis(context.Background(), "MINT", after, before, 500)
	if err != nil {
		t.Fatal(err)
	}
	if page.Count != 663 || len(page.Items) != 25 {
		t.Fatalf("count=%d items=%d", page.Count, len(page.Items))
	}
}

func TestClientRefreshesTokenOn430(t *testing.T) {
	c, tokens := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fresh" {
			w.WriteHeader(430)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		w.Write([]byte(`{"success":true,"responseObject":{"items":[],"hasNextPage":false,"count":1}}`))
	})
	page, err := c.SortedThesis(context.Background(), "MINT", time.Now().Add(-time.Hour), time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if page.Count != 1 || tokens.invalidated.Load() != 1 {
		t.Fatalf("count=%d invalidated=%d", page.Count, tokens.invalidated.Load())
	}
}

func TestClientProxy(t *testing.T) {
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		if r.URL.Host != "fomo.example" {
			t.Errorf("proxy got %s", r.URL)
		}
		w.Write([]byte(`{"success":true,"responseObject":{"items":[],"hasNextPage":false,"count":2}}`))
	}))
	t.Cleanup(proxy.Close)
	tokens := &fakeTokens{}
	tokens.token.Store("fresh")
	c, err := NewClient(Options{BaseURL: "http://fomo.example", RPS: 1000, Proxy: proxy.URL},
		tokens, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	page, err := c.SortedThesis(context.Background(), "MINT", time.Now().Add(-time.Hour), time.Now(), 10)
	if err != nil || page.Count != 2 || proxied.Load() != 1 {
		t.Fatalf("count=%v proxied=%d err=%v", page, proxied.Load(), err)
	}
	if _, err := NewClient(Options{Proxy: "::bad"}, tokens, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("bad proxy URL must be rejected")
	}
}

func TestClientGivesUpAfterSecondAuthFailure(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) })
	if _, err := c.Feed(context.Background(), 50); err == nil {
		t.Fatal("expected unauthorized error")
	}
}

func TestClientRateLimitBackoff(t *testing.T) {
	var calls atomic.Int32
	c, tokens := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"success":true,"responseObject":{"items":[],"hasNextPage":false,"count":5}}`))
	})
	tokens.token.Store("fresh")
	start := time.Now()
	page, err := c.TokenThesis(context.Background(), "MINT", "")
	if err != nil {
		t.Fatal(err)
	}
	if page.Count != 5 || calls.Load() != 3 || c.Stats.RateLimited.Load() != 2 {
		t.Fatalf("count=%d calls=%d 429s=%d", page.Count, calls.Load(), c.Stats.RateLimited.Load())
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Errorf("no backoff between 429 retries (%v)", elapsed)
	}
}

func TestClientETag(t *testing.T) {
	var calls atomic.Int32
	c, tokens := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Write([]byte(`{"success":true,"responseObject":{"feed":[{"id":"1","type":"thesis_created","tokenAddress":"T"}]}}`))
	})
	tokens.token.Store("fresh")
	for i := 0; i < 2; i++ {
		events, err := c.Feed(context.Background(), 50)
		if err != nil || len(events) != 1 {
			t.Fatalf("call %d: events=%d err=%v", i, len(events), err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestProxyEndpointsAndCallHook(t *testing.T) {
	var calls []CallInfo
	var feedHits atomic.Int32
	c, tokens := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/proxy/tokenDetails":
			if r.Method != http.MethodPost || string(body) != `{"tokenId":"MINT:1399811149"}` {
				t.Errorf("tokenDetails %s body %s", r.Method, body)
			}
			w.Write([]byte(`{"success":true,"responseObject":{"name":"X"}}`))
		case "/proxy/filterTokens":
			if string(body) != `["A:1399811149","B:1399811149"]` || r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("filterTokens body %s", body)
			}
			w.Write([]byte(`[{"id":"A"},{"id":"B"}]`))
		case "/feed":
			if feedHits.Add(1) > 1 && r.Header.Get("If-None-Match") == `"v1"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"v1"`)
			w.Write([]byte(`{"success":true,"responseObject":{"feed":[]}}`))
		}
	})
	tokens.token.Store("fresh")
	c.opts.OnCall = func(ci CallInfo) { calls = append(calls, ci) }
	ctx := context.Background()

	details, err := c.TokenDetails(ctx, "MINT")
	if err != nil || string(details) != `{"name":"X"}` {
		t.Fatalf("details = %s, %v", details, err)
	}
	batch, err := c.FilterTokens(ctx, []string{"A", "B"})
	if err != nil || string(batch) != `[{"id":"A"},{"id":"B"}]` {
		t.Fatalf("filterTokens = %s, %v", batch, err)
	}
	if _, nm, err := c.FeedWithStatus(ctx, 50); err != nil || nm {
		t.Fatalf("first feed: notModified=%v err=%v", nm, err)
	}
	if _, nm, err := c.FeedWithStatus(ctx, 50); err != nil || !nm {
		t.Fatalf("second feed must be 304: notModified=%v err=%v", nm, err)
	}
	if len(calls) != 4 || calls[0].Path != "/proxy/tokenDetails" || calls[3].Status != http.StatusNotModified {
		t.Errorf("calls = %+v", calls)
	}
}

type fakePage struct {
	calls atomic.Int32
	body  []byte
}

func (f *fakePage) FetchInPage(_ context.Context, method, rawURL string, hdr map[string]string, body []byte) (int, []byte, map[string]string, error) {
	f.calls.Add(1)
	if method != http.MethodGet || hdr["Authorization"] == "" || hdr["X-Supported-Chains"] == "" {
		return 0, nil, nil, fmt.Errorf("bad page request %s %s %#v", method, rawURL, hdr)
	}
	if hdr["Authorization"] != "Bearer fresh" {
		return 431, []byte(`{"error":"unauthorized"}`), nil, nil
	}
	if !strings.Contains(rawURL, "/feed?") {
		return 0, nil, nil, fmt.Errorf("url %s", rawURL)
	}
	return 200, f.body, map[string]string{"content-type": "application/json"}, nil
}

func TestResponseFacts(t *testing.T) {
	body := []byte(`{"success":true,"responseObject":{"items":[{},{}],"hasNextPage":false,"count":40},"statusCode":200}`)
	items, count, hasNext, status, hit, ok := responseFacts(body, "500")
	if !ok || items != 2 || count != 40 || hasNext || status != 200 || hit {
		t.Fatalf("facts = %d %d %v %d hit=%v ok=%v", items, count, hasNext, status, hit, ok)
	}
	items, _, hasNext, _, hit, ok = responseFacts(body, "2")
	if !ok || items != 2 || !hit || hasNext {
		t.Fatalf("limit hit: items=%d hit=%v ok=%v", items, hit, ok)
	}
	if _, _, _, st, _, ok := responseFacts([]byte(`{"statusCode":401,"responseObject":[]}`), ""); !ok || st != 401 {
		t.Fatalf("error envelope status=%d ok=%v", st, ok)
	}
	if ms, ok := msBetween("2026-10-03T09:00:00.000Z", "2026-10-03T09:00:00.180Z"); !ok || ms != 180 {
		t.Fatalf("ms=%d ok=%v", ms, ok)
	}
}

func TestClientUsesAttachedChrome(t *testing.T) {
	fixture, err := os.ReadFile("testdata/feed_thesis_response.json")
	if err != nil {
		t.Fatal(err)
	}
	var direct atomic.Int32
	c, tokens := newTestClient(t, func(http.ResponseWriter, *http.Request) {
		direct.Add(1)
	})
	page := &fakePage{body: fixture}
	c.page = page
	tokens.token.Store("stale")

	events, err := c.Feed(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 8 || tokens.invalidated.Load() != 1 || page.calls.Load() != 2 || direct.Load() != 0 {
		t.Fatalf("events=%d invalidated=%d page=%d direct=%d", len(events), tokens.invalidated.Load(), page.calls.Load(), direct.Load())
	}
}
