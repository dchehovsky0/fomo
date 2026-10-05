package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"fomobot/internal/notify"
)

func TestResources(t *testing.T) {
	h := Resources()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health/resources", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"goroutines"`) || !strings.Contains(body, `"heap_inuse"`) || !strings.Contains(body, `"cpu_seconds"`) {
		t.Fatalf("body %s", body)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health/resources", nil))
	if !strings.Contains(rec.Body.String(), `"cpu_percent_recent"`) {
		t.Fatalf("second body %s", rec.Body.String())
	}
}

func TestAdminAccess(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("secret report")) })
	get := func(cfg Config, remote, url, auth string) int {
		cfg.Dir = t.TempDir()
		s, err := New(cfg, time.UTC, quiet)
		if err != nil {
			t.Fatal(err)
		}
		s.HandleAdmin("GET /health/accounts", ok)
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req.RemoteAddr = remote
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	local, other := "127.0.0.1:50000", "203.0.113.7:50000"
	cases := []struct {
		name        string
		cfg         Config
		remote, url string
		auth        string
		want        int
	}{
		{"local run, this computer", Config{}, local, "/health/accounts", "", 200},
		{"local run, other machine", Config{}, other, "/health/accounts", "", 403},
		{"server without token", Config{Exposed: true}, local, "/health/accounts", "", 403},
		{"server, no token given", Config{Exposed: true, AdminToken: "s3"}, other, "/health/accounts", "", 401},
		{"server, wrong token", Config{Exposed: true, AdminToken: "s3"}, other, "/health/accounts?token=nope", "", 401},
		{"server, token in query", Config{Exposed: true, AdminToken: "s3"}, other, "/health/accounts?token=s3", "", 200},
		{"server, token in header", Config{Exposed: true, AdminToken: "s3"}, other, "/health/accounts", "s3", 200},
	}
	for _, c := range cases {
		if got := get(c.cfg, c.remote, c.url, c.auth); got != c.want {
			t.Errorf("%s: status %d, want %d", c.name, got, c.want)
		}
	}
}

func TestSaveAndServe(t *testing.T) {
	s, err := New(Config{Dir: t.TempDir(), PublicURL: "http://localhost:8080", Keep: time.Hour},
		time.FixedZone("MSK", 3*3600), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	url, err := s.Save(notify.Alert{
		Kind: notify.KindTheses, Threshold: 3, Token: "MINT", Symbol: "CAT", Name: "Cat <script>", Chain: "Solana",
		CreatedAt: now.Add(-time.Hour), DetectedAt: now, MarketCap: 1_500_000, CountKnown: true, Count: 3,
		RateKnown: true, Recent: 2, RateWindow: 10 * time.Minute, FirstExact: true,
		FomoURL: "https://fomo.family/tokens/solana/MINT", AxiomURL: "https://axiom.trade/meme/POOL?chain=sol",
		First: []notify.Thesis{
			{At: now.Add(-50 * time.Minute), Handle: "alice", Verified: true, PositionUSD: 1234, Text: "line one\n<b>bold?</b>"},
			{At: now.Add(-40 * time.Minute), Handle: "bob", Closed: true, Text: "second"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	path, ok := strings.CutPrefix(url, "http://localhost:8080")
	if !ok || !strings.HasPrefix(path, "/t/") {
		t.Fatalf("url = %s", url)
	}

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, body)
	}
	for _, want := range []string{"$CAT", "Cat &lt;script&gt;", "alice", "line one\n&lt;b&gt;bold?&lt;/b&gt;", "second",
		"$1,234", "https://axiom.trade/meme/POOL?chain=sol", "MINT"} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	if strings.Contains(body, "<b>bold?</b>") {
		t.Error("thesis text must be escaped")
	}

	for _, bad := range []string{"/t/short", "/t/AAAAAAAAAAAAAAAAAAAAAA", "/t/..%2F..%2Fconfig"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, bad, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d", bad, rec.Code)
		}
	}
}

func TestBoardPage(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	serve := func(cfg Config, remote, url string) *httptest.ResponseRecorder {
		t.Helper()
		cfg.Dir = t.TempDir()
		s, err := New(cfg, time.UTC, quiet)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	local := serve(Config{}, "127.0.0.1:50000", "/")
	if local.Code != http.StatusOK || !strings.Contains(local.Body.String(), "Наблюдение") {
		t.Fatalf("local status %d", local.Code)
	}
	if csp := local.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "connect-src 'self'") {
		t.Fatalf("csp %s", csp)
	}
	if local.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("referrer %s", local.Header().Get("Referrer-Policy"))
	}
	if got := serve(Config{Exposed: true, AdminToken: "s3"}, "203.0.113.7:1", "/").Code; got != http.StatusUnauthorized {
		t.Fatalf("open server status %d", got)
	}

	cfg := Config{Dir: t.TempDir(), Exposed: true, AdminToken: "s3", PublicURL: "https://bot.example.com"}
	s, err := New(cfg, time.UTC, quiet)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/?token=s3", nil)
	req.RemoteAddr = "203.0.113.7:1"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("redirect %d %s", rec.Code, rec.Header().Get("Location"))
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == adminCookie {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value != "s3" || !cookie.HttpOnly || !cookie.Secure {
		t.Fatalf("cookie %+v", cookie)
	}
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.7:1"
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "/api/board") {
		t.Fatalf("with cookie %d", rec.Code)
	}
}

func TestAdminCookie(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := New(Config{Dir: t.TempDir(), Exposed: true, AdminToken: "s3"}, time.UTC, quiet)
	if err != nil {
		t.Fatal(err)
	}
	s.HandleAdmin("GET /health/accounts", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) }))
	req := httptest.NewRequest(http.MethodGet, "/health/accounts", nil)
	req.RemoteAddr = "203.0.113.7:1"
	req.AddCookie(&http.Cookie{Name: adminCookie, Value: "s3"})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
}
