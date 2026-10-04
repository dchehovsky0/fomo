package session

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func makeJWT(exp time.Time) string {
	enc := base64.RawURLEncoding.EncodeToString
	payload := fmt.Sprintf(`{"iss":"privy.io","aud":"cm6h485o300n3zj9yl6vpedq7","exp":%d}`, exp.Unix())
	return enc([]byte(`{"alg":"ES256","typ":"JWT"}`)) + "." + enc([]byte(payload)) + ".sig"
}

func TestNormalizeAndExpiry(t *testing.T) {
	exp := time.Now().Add(time.Hour).Truncate(time.Second)
	jwt := makeJWT(exp)
	for _, raw := range []string{jwt, `"` + jwt + `"`, "Bearer " + jwt, "  \"" + jwt + "\"\n"} {
		got := Normalize(raw)
		if got != jwt {
			t.Fatalf("Normalize(%q) = %q", raw, got)
		}
		e, err := ParseExpiry(got)
		if err != nil || !e.Equal(exp) {
			t.Fatalf("ParseExpiry = %v, %v; want %v", e, err, exp)
		}
	}
	if _, err := ParseExpiry("not-a-jwt"); err == nil {
		t.Error("expected error for non-JWT")
	}
}

func TestStaticTokenFileReload(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "token.txt")
	first := makeJWT(time.Now().Add(time.Hour))
	if err := os.WriteFile(path, []byte(`"`+first+`"`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewStatic("", path, log)
	got, err := s.Token(context.Background())
	if err != nil || got != first {
		t.Fatalf("Token = %q, %v", got, err)
	}

	second := makeJWT(time.Now().Add(2 * time.Hour))
	if err := os.WriteFile(path, []byte(second), 0o600); err != nil {
		t.Fatal(err)
	}
	s.Invalidate()
	if got, _ := s.Token(context.Background()); got != second {
		t.Fatalf("token not reloaded from file")
	}
}

func TestChromeDoesNotRelaunchWithoutLogin(t *testing.T) {
	c, err := NewChrome(ChromeOptions{ProfileDir: t.TempDir(), RefreshBefore: 5 * time.Minute}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	launches := 0
	c.fetch = func(context.Context) (string, time.Time, error) {
		launches++
		return "", time.Time{}, fmt.Errorf("%w: not logged in", ErrNoToken)
	}
	for range 5 {
		if _, err := c.Token(context.Background()); !errors.Is(err, ErrNoToken) {
			t.Fatalf("err = %v, want ErrNoToken", err)
		}
	}
	c.Invalidate()
	c.Token(context.Background())
	if launches != 1 {
		t.Fatalf("chrome launched %d times, want 1 until the retry pause ends", launches)
	}

	c.failUntil = time.Now().Add(-time.Second)
	tok := makeJWT(time.Now().Add(time.Hour))
	c.fetch = func(context.Context) (string, time.Time, error) { return tok, time.Now().Add(time.Hour), nil }
	if got, err := c.Token(context.Background()); err != nil || got != tok {
		t.Fatalf("after login: %q, %v", got, err)
	}
}

type fakeSource struct {
	tok         string
	invalidated int
}

func (f *fakeSource) Token(context.Context) (string, error) {
	if f.tok == "" {
		return "", ErrNoToken
	}
	return f.tok, nil
}

func (f *fakeSource) Invalidate() { f.invalidated++ }

func TestFailover(t *testing.T) {
	dead, a, b := &fakeSource{}, &fakeSource{tok: "A"}, &fakeSource{tok: "B"}
	f := NewFailover(dead, a, b)
	if got, err := f.Token(context.Background()); err != nil || got != "A" {
		t.Fatalf("Token = %q, %v", got, err)
	}
	f.Invalidate()
	if a.invalidated != 1 {
		t.Error("rejection must reach the account that gave the token")
	}
	if got, _ := f.Token(context.Background()); got != "B" {
		t.Fatalf("after invalidate: %q, want B", got)
	}
	if _, err := NewFailover(dead).Token(context.Background()); !errors.Is(err, ErrNoToken) {
		t.Errorf("all dead: %v", err)
	}
}

func TestStaticExpired(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := NewStatic(makeJWT(time.Now().Add(-time.Minute)), "", log)
	if _, err := s.Token(context.Background()); err == nil {
		t.Fatal("expected error for expired token")
	}
}

func TestInPageRequiresDebugURL(t *testing.T) {
	c, err := NewChrome(ChromeOptions{ProfileDir: t.TempDir()}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if c.InPage() {
		t.Fatal("profile visit must keep API calls on the Go client")
	}
	c.opts.DebugURL = "http://127.0.0.1:9221"
	if !c.InPage() {
		t.Fatal("debug_url attaches and runs API calls in the tab")
	}
}

func TestFindFomoTargetPrefersTokenPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/list" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`[
		  {"id":"iframe","type":"iframe","url":"https://fomo.family/token"},
		  {"id":"other","type":"page","url":"https://example.com/"},
		  {"id":"feed","type":"page","url":"https://fomo.family/feed"},
		  {"id":"token","type":"page","url":"https://fomo.family/tokens/solana/abc"}
		]`))
	}))
	defer srv.Close()

	got, err := findFomoTarget(context.Background(), srv.URL, "https://fomo.family/", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != "token" {
		t.Fatalf("target = %+v, want the /tokens page", got)
	}
}

func TestFindFomoTargetFallsBackToAnyFomoPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"feed","type":"page","url":"https://fomo.family/feed"}]`))
	}))
	defer srv.Close()
	got, err := findFomoTarget(context.Background(), srv.URL, "https://fomo.family/", time.Second)
	if err != nil || got == nil || got.ID != "feed" {
		t.Fatalf("target = %+v, err = %v", got, err)
	}
}
