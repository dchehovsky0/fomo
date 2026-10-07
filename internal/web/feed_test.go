package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fomobot/internal/domain"
	"fomobot/internal/notify"
)

func TestFeedListsSavedAlerts(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	s, err := New(Config{Dir: dir, PublicURL: "http://localhost:8080"}, time.UTC, quiet)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	save := func(a notify.Alert) string {
		t.Helper()
		u, err := s.Save(a)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimPrefix(u, "http://localhost:8080")
	}
	oldPage := save(notify.Alert{
		Kind: notify.KindTheses, Token: "MINTOLD", Symbol: "OLD", Name: "Old coin", Dex: "Pump V1",
		DetectedAt: now.Add(-48 * time.Hour), SentAt: now.Add(-48 * time.Hour),
		Count: 4, Volume: domain.Volume{USD5m: 300, USD1h: 12_000}, VolumeAt: now.Add(-48 * time.Hour),
		FomoURL: "https://fomo.family/tokens/solana/MINTOLD",
		First:   []notify.Thesis{{At: now.Add(-49 * time.Hour), Handle: "bob", Text: "единственный текст"}},
	})
	newPage := save(notify.Alert{
		Kind: notify.KindTheses, Token: "MINTNEW", Symbol: "NEW", Name: "New coin", Chain: "Solana", Tier: 1,
		DetectedAt: now.Add(-time.Hour), SentAt: now.Add(-time.Hour),
		Count: 3, Volume: domain.Volume{USD5m: 8_000, USD1h: 80_000, Trades5m: 90}, VolumeAt: now.Add(-time.Hour), FomoMS: 60188, AxiomURL: "https://axiom.trade/meme/POOL?chain=sol",
		Latest: []notify.Thesis{
			{At: now.Add(-40 * time.Minute), Handle: "a", Text: "старый\nтекст"},
			{At: now.Add(-20 * time.Minute), Handle: "b", Text: "средний"},
			{At: now.Add(-5 * time.Minute), Handle: "c", Text: "свежий"},
		},
	})
	if err := os.WriteFile(filepath.Join(dir, "not-an-id.json"), []byte(`{"symbol":"NO"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	broken := "aaaaaaaaaaaaaaaaaa"
	if err := os.WriteFile(filepath.Join(dir, broken+".json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}

	feed := s.Feed(now)
	if len(feed.Alerts) != 2 {
		t.Fatalf("alerts = %d", len(feed.Alerts))
	}
	if feed.Alerts[0].Symbol != "NEW" || feed.Alerts[0].PageURL != newPage || feed.Alerts[0].Theses != 3 || feed.Alerts[0].USD1h != 80_000 || feed.Alerts[0].USD5m != 8_000 || feed.Alerts[0].VolumeAt.IsZero() || feed.Alerts[0].Tier != 1 || feed.Alerts[0].FomoMS != 60188 {
		t.Fatalf("newest: %+v", feed.Alerts[0])
	}
	if n := feed.Alerts[0].Snippets; len(n) != 3 || n[0].Text != "свежий" || n[2].Text != "старый текст" {
		t.Fatalf("snippets: %+v", n)
	}
	if feed.Alerts[1].Symbol != "OLD" || feed.Alerts[1].PageURL != oldPage || feed.Alerts[1].Mint != "MINTOLD" {
		t.Fatalf("older: %+v", feed.Alerts[1])
	}
	if n := feed.Alerts[1].Snippets; len(n) != 1 || n[0].Text != "единственный текст" {
		t.Fatalf("fallback snippets: %+v", n)
	}
	again := s.Feed(now)
	if again.Alerts[0].ID != feed.Alerts[0].ID || again.Alerts[1].USD1h != 12_000 {
		t.Fatalf("cached feed: %+v", again.Alerts)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/alerts", nil)
	req.RemoteAddr = "127.0.0.1:9"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var got Feed
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Alerts) != 2 || got.Alerts[0].AxiomURL == "" || got.Alerts[0].Trades5m != 90 {
		t.Fatalf("api: %+v", got.Alerts)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"volume_5m":8000`) || !strings.Contains(body, `"volume_1h":80000`) || strings.Contains(body, "market_cap") {
		t.Fatalf("api json: %s", body)
	}

	locked := httptest.NewRequest(http.MethodGet, "/api/alerts", nil)
	locked.RemoteAddr = "203.0.113.7:9"
	rec = httptest.NewRecorder()
	srv, err := New(Config{Dir: dir, Exposed: true, AdminToken: "s3"}, time.UTC, quiet)
	if err != nil {
		t.Fatal(err)
	}
	srv.Handler().ServeHTTP(rec, locked)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("open status %d", rec.Code)
	}
}

func TestDismissHidesCallFromFeed(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	s, err := New(Config{Dir: dir, PublicURL: "http://localhost:8080"}, time.UTC, quiet)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	u, err := s.Save(notify.Alert{
		Kind: notify.KindTheses, Token: "MINT", Symbol: "MEW", Name: "cat",
		DetectedAt: now, SentAt: now, Count: 3,
		First: []notify.Thesis{{At: now, Text: "кол про кота"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimPrefix(u, "http://localhost:8080/t/")
	if len(s.Feed(now).Alerts) != 1 {
		t.Fatal("expected the call in the feed")
	}
	post := func(path, addr string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	if rec := post("/api/alerts/"+id+"/dismiss", "127.0.0.1:9"); rec.Code != http.StatusNoContent {
		t.Fatalf("dismiss %d", rec.Code)
	}
	if n := len(s.Feed(now).Alerts); n != 0 {
		t.Fatalf("feed after dismiss = %d", n)
	}
	pageReq := httptest.NewRequest(http.MethodGet, "/t/"+id, nil)
	pageRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(pageRec, pageReq)
	if pageRec.Code != http.StatusOK || !strings.Contains(pageRec.Body.String(), "кол про кота") {
		t.Fatalf("page %d", pageRec.Code)
	}
	if rec := post("/api/alerts/"+id+"/dismiss", "127.0.0.1:9"); rec.Code != http.StatusNoContent {
		t.Fatalf("second dismiss %d", rec.Code)
	}
	if rec := post("/api/alerts/aaaaaaaaaaaaaaaa/dismiss", "127.0.0.1:9"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing %d", rec.Code)
	}
	open, err := New(Config{Dir: dir, Exposed: true, AdminToken: "s3", PublicURL: "http://localhost:8080"}, time.UTC, quiet)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/alerts/"+id+"/dismiss", nil)
	req.RemoteAddr = "203.0.113.7:9"
	rec := httptest.NewRecorder()
	open.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("open dismiss %d", rec.Code)
	}
	if n := len(open.Feed(now).Alerts); n != 0 {
		t.Fatalf("other server still shows %d", n)
	}
}

func TestTokenDetailListsTheses(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	s, err := New(Config{Dir: dir, PublicURL: "http://localhost:8080"}, time.UTC, quiet)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	u, err := s.Save(notify.Alert{
		Kind: notify.KindTheses, Token: "MINTMEW", Symbol: "MEW", Name: "cat", Chain: "Solana", Tier: 1,
		DetectedAt: now, SentAt: now, Count: 4, Volume: domain.Volume{USD5m: 2_400, USD1h: 24_000}, VolumeAt: now,
		CreatedAt: now.Add(-2 * time.Hour),
		Theses: []notify.Thesis{
			{At: now.Add(-time.Hour), Handle: "early", Text: "ранний тезис", PositionUSD: 10},
			{At: now.Add(-time.Minute), Handle: "late", Text: "свежий тезис", PositionUSD: 5000},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimPrefix(u, "http://localhost:8080/t/")
	old, err := s.Save(notify.Alert{
		Kind: notify.KindTheses, Token: "MINTOLD", Symbol: "OLD", Name: "old",
		DetectedAt: now.Add(-time.Hour), SentAt: now.Add(-time.Hour), Count: 2,
		First:  []notify.Thesis{{At: now.Add(-2 * time.Hour), Handle: "a", Text: "первый", PositionUSD: 100}},
		Latest: []notify.Thesis{{At: now.Add(-2 * time.Hour), Handle: "a", Text: "первый", PositionUSD: 100}, {At: now.Add(-30 * time.Minute), Handle: "b", Text: "второй", PositionUSD: 20}},
	})
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) TokenDetail {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "127.0.0.1:9"
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status %d", path, rec.Code)
		}
		var d TokenDetail
		if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	d := get("/api/alerts/" + id)
	if d.Symbol != "MEW" || d.Mint != "MINTMEW" || d.Count != 4 || d.USD1h != 24_000 || d.USD5m != 2_400 || len(d.Theses) != 2 || d.Theses[0].PositionUSD != 10 || d.Theses[1].Text != "свежий тезис" {
		t.Fatalf("detail: %+v", d)
	}
	oldID := strings.TrimPrefix(old, "http://localhost:8080/t/")
	got := get("/api/alerts/" + oldID)
	if len(got.Theses) != 2 || got.Theses[0].Text != "первый" || got.Theses[1].PositionUSD != 20 {
		t.Fatalf("merged: %+v", got.Theses)
	}
}

func TestDiscardRemovesFailedSend(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	s, err := New(Config{Dir: dir, PublicURL: "http://localhost:8080"}, time.UTC, quiet)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	pageURL, err := s.Save(notify.Alert{
		Kind: notify.KindTheses, Token: "MINT", Symbol: "ELON", Name: "Elon", Dex: "Pump AMM",
		DetectedAt: now, SentAt: now, Count: 7,
		First: []notify.Thesis{{At: now, Handle: "a", Text: "текст"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Feed(now).Alerts) != 1 {
		t.Fatal("saved page must be in the feed before the send result is known")
	}
	if err := s.Discard(pageURL); err != nil {
		t.Fatal(err)
	}
	if len(s.Feed(now).Alerts) != 0 {
		t.Fatal("a failed send must not stay in the feed")
	}
	id := strings.TrimPrefix(pageURL, "http://localhost:8080/t/")
	if _, err := os.Stat(filepath.Join(dir, id+".json")); !os.IsNotExist(err) {
		t.Fatalf("page file: %v", err)
	}
	if err := s.Discard(pageURL); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(dir, "dismissed.json")
	if err := os.WriteFile(kept, []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Discard("http://localhost:8080/t/dismissed"); err != nil {
		t.Fatal(err)
	}
	if err := s.Discard("http://localhost:8080/t/../../config.yaml"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Fatal(err)
	}
}
