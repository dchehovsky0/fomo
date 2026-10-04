package accounts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fomobot/internal/fomo"
	"fomobot/internal/session"
)

func call(a *Account, status int, err error) {
	a.ObserveCall(fomo.CallInfo{At: time.Now(), Status: status, Err: err})
}

type source struct {
	tok string
	err error
}

func (s *source) Token(context.Context) (string, error) { return s.tok, s.err }
func (s *source) Invalidate()                           {}

func jwt(exp time.Time) string {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return enc(`{"alg":"ES256"}`) + "." + enc(fmt.Sprintf(`{"exp":%d}`, exp.Unix())) + ".sig"
}

func TestStates(t *testing.T) {
	r := NewRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)))
	a := r.Add("acc1", "socks5://user:secret@1.2.3.4:1080", 1)
	now := func() time.Time { return time.Now().Add(time.Millisecond) }

	if s := a.Status(now()); s.State != StateUnknown || s.Healthy || s.Proxy != "socks5://1.2.3.4:1080" {
		t.Fatalf("new account: %+v", s)
	}

	src := &source{err: fmt.Errorf("%w: privy:token not found", session.ErrNoToken)}
	tokens := a.Track(src)
	tokens.Token(context.Background())
	if s := a.Status(now()); s.State != StateNeedsLogin || s.Healthy || !strings.Contains(s.Reason, "fomobot -login -account acc1") {
		t.Fatalf("no login: %+v", s)
	}

	exp := time.Now().Add(time.Hour).Truncate(time.Second)
	src.tok, src.err = jwt(exp), nil
	tokens.Token(context.Background())
	call(a, 200, nil)
	if s := a.Status(now()); s.State != StateOK || !s.Healthy || !s.TokenExpiresAt.Equal(exp) || s.Requests != 1 {
		t.Fatalf("after login: %+v", s)
	}

	call(a, 430, nil)
	if s := a.Status(now()); s.State != StateRejected || s.Healthy {
		t.Fatalf("rejected: %+v", s)
	}
	call(a, 200, nil) // the client refreshed the token and the retry passed
	if s := a.Status(now()); s.State != StateOK {
		t.Fatalf("refreshed: %+v", s)
	}

	call(a, 429, nil)
	if s := a.Status(now()); s.State != StateRateLimited || !s.Healthy {
		t.Fatalf("429 right after a success must stay healthy: %+v", s)
	}

	for range failingAfter {
		call(a, 0, errors.New("proxyconnect tcp: connection refused"))
	}
	s := a.Status(now())
	if s.State != StateFailing || s.Healthy || !strings.Contains(s.Reason, "прокси") || s.ConsecutiveErrors != failingAfter {
		t.Fatalf("proxy down: %+v", s)
	}
	if strings.Contains(fmt.Sprintf("%+v", s), "secret") {
		t.Fatal("proxy password must not be shown")
	}

	call(a, 0, context.Canceled) // shutdown, not the account's fault
	if got := a.Status(now()).ConsecutiveErrors; got != failingAfter {
		t.Errorf("canceled request counted as an error: %d", got)
	}
}

func TestHandlerAndProbe(t *testing.T) {
	r := NewRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)))
	good, bad := r.Add("good", "", 1), r.Add("bad", "", 1)
	var probes atomic.Int64
	good.SetProbe(func(context.Context) error { probes.Add(1); call(good, 200, nil); return nil })
	bad.SetProbe(func(context.Context) error {
		probes.Add(1)
		bad.observeToken("", fmt.Errorf("%w: not logged in", session.ErrNoToken))
		return session.ErrNoToken
	})

	get := func(url string) (int, Report) {
		rec := httptest.NewRecorder()
		r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		var rep Report
		if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
			t.Fatalf("%s: %v\n%s", url, err, rec.Body.String())
		}
		return rec.Code, rep
	}

	if code, rep := get("/health/accounts"); code != http.StatusServiceUnavailable || rep.Healthy != 0 || probes.Load() != 0 {
		t.Fatalf("before probe: %d %+v", code, rep)
	}
	code, rep := get("/health/accounts?probe=1")
	if code != http.StatusServiceUnavailable || rep.Total != 2 || rep.Healthy != 1 || len(rep.Unhealthy) != 1 || rep.Unhealthy[0] != "bad" {
		t.Fatalf("after probe: %d %+v", code, rep)
	}
	if rep.Accounts[1].State != StateNeedsLogin {
		t.Errorf("bad = %+v", rep.Accounts[1])
	}

	// Run probes only accounts that had no recent success.
	probes.Store(0)
	ctx, cancel := context.WithCancel(context.Background())
	good.mu.Lock()
	good.lastProbe = time.Time{}
	good.mu.Unlock()
	call(good, 200, nil)
	bad.mu.Lock()
	bad.lastProbe = time.Time{}
	bad.mu.Unlock()
	done := make(chan struct{})
	go func() { r.Run(ctx, time.Hour, time.Minute); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for probes.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if probes.Load() != 1 {
		t.Errorf("probes = %d, want only the idle account", probes.Load())
	}

	bad.SetProbe(func(context.Context) error { call(bad, 200, nil); return nil })
	if code, rep := get("/health/accounts?probe=1"); code != http.StatusOK || rep.Healthy != 2 {
		t.Errorf("all good: %d %+v", code, rep)
	}
}

func TestHandlerReportsTokensInWork(t *testing.T) {
	r := NewRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.Add("a", "", 1)
	r.SetWatching(func() int { return 97 })
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health/accounts", nil))
	var rep Report
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Watching != 97 {
		t.Fatalf("watching = %d", rep.Watching)
	}
}

func TestHandlerIncludesDexScreener(t *testing.T) {
	r := NewRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.Add("a", "", 1)
	r.SetDexScreener(func() any { return map[string]int{"tokens": 3} })
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health/accounts", nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	dex, _ := body["dexscreener"].(map[string]any)
	if dex["tokens"] != float64(3) {
		t.Fatalf("dexscreener = %v", body["dexscreener"])
	}
}
