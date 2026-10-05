package notify

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func quietNotify() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type memPages struct {
	mu      sync.Mutex
	saved   int
	dropped []string
}

func (m *memPages) Save(Alert) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saved++
	return "http://localhost:8080/t/abcdefghijklmnopqrst", nil
}

func (m *memPages) Discard(pageURL string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dropped = append(m.dropped, pageURL)
	return nil
}

func (m *memPages) droppedURLs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.dropped...)
}

func thesisAlert() Alert {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	return Alert{
		Kind: KindTheses, Token: "MINT", Symbol: "ELON", Name: "Elon", Dex: "Pump AMM",
		DetectedAt: now, Count: 7, MarketCap: 46_000,
		First: []Thesis{{At: now, Handle: "a", Text: "текст"}},
	}
}

func TestFailedTelegramSendDiscardsThePage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: BUTTON_URL_INVALID"}`))
	}))
	defer srv.Close()

	pages := &memPages{}
	tg := NewTelegram("token", "chat", time.UTC, pages, quietNotify())
	tg.APIBase = srv.URL
	if err := tg.Send(context.Background(), thesisAlert()); err == nil {
		t.Fatal("expected telegram to reject the message")
	}
	got := pages.droppedURLs()
	if pages.saved != 1 || len(got) != 1 || got[0] != "http://localhost:8080/t/abcdefghijklmnopqrst" {
		t.Fatalf("saved %d, discarded %v", pages.saved, got)
	}
}

func TestButtonFallbackKeepsThePage(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: BUTTON_URL_INVALID"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer srv.Close()

	pages := &memPages{}
	tg := NewTelegram("token", "chat", time.UTC, pages, quietNotify())
	tg.APIBase = srv.URL
	if err := tg.Send(context.Background(), thesisAlert()); err != nil {
		t.Fatal(err)
	}
	if pages.saved != 1 || len(pages.droppedURLs()) != 0 {
		t.Fatalf("saved %d, discarded %v", pages.saved, pages.droppedURLs())
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestFailedPrintDiscardsThePage(t *testing.T) {
	pages := &memPages{}
	c := NewConsole(failWriter{}, time.UTC, pages, quietNotify())
	if err := c.Send(context.Background(), thesisAlert()); err == nil {
		t.Fatal("expected the print to fail")
	}
	if pages.saved != 1 || len(pages.droppedURLs()) != 1 {
		t.Fatalf("saved %d, discarded %v", pages.saved, pages.droppedURLs())
	}
	ok := NewConsole(&bytes.Buffer{}, time.UTC, pages, quietNotify())
	if err := ok.Send(context.Background(), thesisAlert()); err != nil {
		t.Fatal(err)
	}
	if len(pages.droppedURLs()) != 1 {
		t.Fatalf("a printed alert must keep its page, discarded %v", pages.droppedURLs())
	}
}
