package fomo

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"fomobot/internal/session"
)

// WS is one fomo websocket: dial, answer the challenge with the account
// token, subscribe, then hand every frame to OnFrame. Both the trending
// alerts and the recorder use it.
type WS struct {
	URL       string
	Origin    string
	UserAgent string
	TopicType string
	TopicID   string
	// Idle reconnects when no frame arrives for this long. Zero waits
	// until the context ends.
	Idle   time.Duration
	Tokens session.TokenSource
	Log    *slog.Logger
}

// Run serves one connection. OnFrame sees every frame, including the
// challenge frames, before the handshake reply is sent. A non-nil error
// from OnFrame ends the connection.
func (w WS) Run(ctx context.Context, onFrame func(typ string, data []byte) error) error {
	hdr := http.Header{}
	if w.Origin != "" {
		hdr.Set("Origin", w.Origin)
	}
	if w.UserAgent != "" {
		hdr.Set("User-Agent", w.UserAgent)
	}
	dialStart := time.Now()
	dialCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	conn, _, err := websocket.Dial(dialCtx, w.URL, &websocket.DialOptions{HTTPHeader: hdr})
	cancel()
	if err != nil {
		if w.Log != nil {
			w.Log.Warn("fomo websocket dial failed", "topic", w.TopicType,
				"took", time.Since(dialStart).Round(time.Millisecond), "err", err)
		}
		return fmt.Errorf("dial: %w", err)
	}
	if w.Log != nil {
		w.Log.Info("fomo websocket dialed", "topic", w.TopicType, "took", time.Since(dialStart).Round(time.Millisecond))
	}
	defer conn.CloseNow()
	conn.SetReadLimit(64 << 20)
	connected := time.Now()
	var challenged time.Time

	for {
		readStart := time.Now()
		data, err := readFrame(ctx, conn, w.Idle)
		if err != nil {
			if w.Log != nil && ctx.Err() == nil {
				w.Log.Warn("fomo websocket read failed", "topic", w.TopicType,
					"waited", time.Since(readStart).Round(time.Millisecond),
					"up_for", time.Since(connected).Round(time.Second), "err", err)
			}
			return err
		}
		var head struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(data, &head)
		if onFrame != nil {
			if err := onFrame(head.Type, data); err != nil {
				return err
			}
		}
		switch head.Type {
		case "challenge":
			challenged = time.Now()
			tokenStart := time.Now()
			tok, err := w.Tokens.Token(ctx)
			tokenWait := time.Since(tokenStart)
			if err != nil {
				if w.Log != nil {
					w.Log.Warn("fomo websocket challenge failed", "topic", w.TopicType,
						"token_wait", tokenWait.Round(time.Millisecond),
						"since_dial", time.Since(connected).Round(time.Millisecond), "err", err)
				}
				return fmt.Errorf("token for websocket: %w", err)
			}
			if w.Log != nil {
				w.Log.Info("fomo websocket challenge answered", "topic", w.TopicType,
					"token_wait", tokenWait.Round(time.Millisecond),
					"since_dial", time.Since(connected).Round(time.Millisecond))
			}
			if err := writeJSON(ctx, conn, map[string]string{"type": "challengeResponse", "jwt": tok}); err != nil {
				return err
			}
		case "challengeAccepted":
			if err := writeJSON(ctx, conn, map[string]string{
				"type": "subscribe", "topicType": w.TopicType, "topicId": w.TopicID,
			}); err != nil {
				return err
			}
			if w.Log != nil {
				sinceChallenge := time.Duration(0)
				if !challenged.IsZero() {
					sinceChallenge = time.Since(challenged)
				}
				w.Log.Info("subscribed", "topic", w.TopicType,
					"since_dial", time.Since(connected).Round(time.Millisecond),
					"since_challenge", sinceChallenge.Round(time.Millisecond))
			}
		}
	}
}

func readFrame(ctx context.Context, conn *websocket.Conn, idle time.Duration) ([]byte, error) {
	rctx, cancel := ctx, func() {}
	if idle > 0 {
		rctx, cancel = context.WithTimeout(ctx, idle)
	}
	defer cancel()
	_, data, err := conn.Read(rctx)
	return data, err
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, b)
}
