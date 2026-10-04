package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Pages publishes the full theses of an alert and returns the page URL.
type Pages interface {
	Save(a Alert) (string, error)
}

type Telegram struct {
	APIBase string
	token   string
	chatID  string
	loc     *time.Location
	pages   Pages
	httpc   *http.Client
	log     *slog.Logger
}

// NewTelegram: pages may be nil, then alerts go without the theses button.
func NewTelegram(token, chatID string, loc *time.Location, pages Pages, log *slog.Logger) *Telegram {
	return &Telegram{
		APIBase: "https://api.telegram.org",
		token:   token,
		chatID:  chatID,
		loc:     loc,
		pages:   pages,
		httpc:   &http.Client{Timeout: 15 * time.Second},
		log:     log,
	}
}

func (t *Telegram) Send(ctx context.Context, a Alert) error {
	started := time.Now()
	a.SentAt = started
	a.PageURL = publishPage(t.pages, a, t.log)
	text, button := Format(a, t.loc), pageButton(a)
	err := t.send(ctx, text, button)
	var apiErr *apiError
	if button != nil && errors.As(err, &apiErr) && apiErr.Status == http.StatusBadRequest {
		// Telegram refuses buttons to addresses like localhost; a plain URL
		// in the text still opens in the browser of this computer.
		t.log.Info("telegram rejected the theses button, putting the link into the text", "url", a.PageURL, "err", err)
		err = t.send(ctx, text+"\n\n📖 Тексты тезисов: "+html.EscapeString(a.PageURL), nil)
	}
	if err != nil {
		t.log.Warn("telegram send failed", "token", a.Token, "kind", a.Kind,
			"took", time.Since(started).Round(time.Millisecond), "err", err)
		return err
	}
	t.log.Info("telegram send", "token", a.Token, "kind", a.Kind, "took", time.Since(started).Round(time.Millisecond))
	return nil
}

type apiError struct {
	Status      int
	Description string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("telegram: HTTP %d: %s", e.Status, e.Description)
}

func (t *Telegram) SendText(ctx context.Context, text string) error {
	return t.send(ctx, text, nil)
}

func publishPage(pages Pages, a Alert, log *slog.Logger) string {
	if pages == nil || len(a.First) == 0 {
		return ""
	}
	u, err := pages.Save(a)
	if err != nil {
		log.Warn("save theses page, alert goes without the button", "token", a.Token, "err", err)
		return ""
	}
	return u
}

type inlineButton struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

func pageButton(a Alert) any {
	if a.PageURL == "" {
		return nil
	}
	text := fmt.Sprintf("📖 Тексты первых %d %s", len(a.First), plural(len(a.First), "тезиса", "тезисов", "тезисов"))
	return map[string]any{"inline_keyboard": [][]inlineButton{{{Text: text, URL: a.PageURL}}}}
}

type tgResponse struct {
	OK          bool   `json:"ok"`
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

func (t *Telegram) send(ctx context.Context, text string, markup any) error {
	msg := map[string]any{
		"chat_id":              t.chatID,
		"text":                 text,
		"parse_mode":           "HTML",
		"link_preview_options": map[string]bool{"is_disabled": true},
	}
	if markup != nil {
		msg["reply_markup"] = markup
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/bot%s/sendMessage", t.APIBase, t.token)

	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		reqStart := time.Now()
		resp, err := t.httpc.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// The URL contains the bot token; don't let it leak into logs.
			lastErr = fmt.Errorf("telegram request failed: %s", redact(err.Error(), t.token))
			t.log.Warn("telegram api failed", "attempt", attempt+1,
				"took", time.Since(reqStart).Round(time.Millisecond), "err", lastErr)
			if err := wait(ctx, time.Duration(1<<attempt)*time.Second); err != nil {
				return err
			}
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()

		var r tgResponse
		_ = json.Unmarshal(body, &r)
		if resp.StatusCode == http.StatusOK && r.OK {
			t.log.Info("telegram api", "status", resp.StatusCode, "attempt", attempt+1,
				"took", time.Since(reqStart).Round(time.Millisecond), "bytes", len(body))
			return nil
		}
		lastErr = &apiError{Status: resp.StatusCode, Description: r.Description}
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			d := time.Duration(max(r.Parameters.RetryAfter, 1)) * time.Second
			t.log.Warn("telegram rate limited", "retry_after", d)
			if err := wait(ctx, d); err != nil {
				return err
			}
		case resp.StatusCode >= 500:
			if err := wait(ctx, time.Duration(1<<attempt)*time.Second); err != nil {
				return err
			}
		default:
			return lastErr
		}
	}
	return lastErr
}

// Console prints alerts to a writer instead of sending them (dry run).
type Console struct {
	mu    sync.Mutex
	w     io.Writer
	loc   *time.Location
	pages Pages
	log   *slog.Logger
}

func NewConsole(w io.Writer, loc *time.Location, pages Pages, log *slog.Logger) *Console {
	return &Console{w: w, loc: loc, pages: pages, log: log}
}

func (c *Console) Send(_ context.Context, a Alert) error {
	started := time.Now()
	a.SentAt = started
	a.PageURL = publishPage(c.pages, a, c.log)
	c.mu.Lock()
	defer c.mu.Unlock()
	text := Format(a, c.loc)
	if a.PageURL != "" {
		text += "\n[кнопка] " + a.PageURL
	}
	_, err := fmt.Fprintf(c.w, "\n===== DRY-RUN ALERT =====\n%s\n=========================\n\n", text)
	if err != nil {
		c.log.Warn("alert print failed", "token", a.Token, "kind", a.Kind,
			"took", time.Since(started).Round(time.Millisecond), "err", err)
		return err
	}
	c.log.Info("alert printed", "token", a.Token, "kind", a.Kind, "took", time.Since(started).Round(time.Millisecond))
	return nil
}

func wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return string(bytes.ReplaceAll([]byte(s), []byte(secret), []byte("<redacted>")))
}
