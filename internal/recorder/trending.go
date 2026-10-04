package recorder

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"fomobot/internal/db"
	"fomobot/internal/fomo"
	"fomobot/internal/session"
)

type TrendingConfig struct {
	URL       string
	TopicID   string
	Origin    string
	UserAgent string
	NetworkID string
	// Sample: how often the tokens listed since the previous sample are stored.
	Sample time.Duration
}

// Trending follows the trending_tokens WebSocket topic
// (challenge -> challengeResponse{jwt} -> challengeAccepted -> subscribe).
type Trending struct {
	cfg    TrendingConfig
	tokens session.TokenSource
	rec    *Recorder
	db     *db.DB
	log    *slog.Logger

	mu        sync.Mutex
	latest    json.RawMessage
	order     []string
	listed    map[string]bool
	lastSaved map[string]time.Time // control message type -> last stored
	messages  int64
}

func (t *Trending) MessageCount() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.messages
}

func NewTrending(cfg TrendingConfig, tokens session.TokenSource, rec *Recorder, store *db.DB, log *slog.Logger) *Trending {
	return &Trending{cfg: cfg, tokens: tokens, rec: rec, db: store, log: log,
		listed: map[string]bool{}, lastSaved: map[string]time.Time{}}
}

func (t *Trending) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { t.sampleLoop(ctx) })
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := t.session(ctx)
		if ctx.Err() != nil {
			break
		}
		if time.Since(start) > 2*time.Minute {
			backoff = 5 * time.Second
		}
		t.log.Warn("trending websocket disconnected", "err", err, "retry_in", backoff)
		if sleep(ctx, backoff) != nil {
			break
		}
		backoff = min(backoff*2, 5*time.Minute)
	}
	wg.Wait()
}

func (t *Trending) session(ctx context.Context) error {
	subscribed := false
	err := fomo.WS{
		URL: t.cfg.URL, Origin: t.cfg.Origin, UserAgent: t.cfg.UserAgent,
		TopicType: "trending_tokens", TopicID: t.cfg.TopicID,
		Tokens: t.tokens, Log: t.log,
	}.Run(ctx, func(typ string, data []byte) error {
		now := time.Now()
		t.mu.Lock()
		t.messages++
		t.mu.Unlock()
		if typ == "challengeAccepted" {
			subscribed = true
		}
		mints := solanaMints(data, t.cfg.NetworkID)
		if len(mints) == 0 {
			t.saveControl(ctx, now, typ, data)
			if typ != "challenge" && typ != "challengeAccepted" && !subscribed {
				t.log.Info("websocket message before subscription", "type", typ, "bytes", len(data))
			}
			return nil
		}
		t.mu.Lock()
		t.latest = append(t.latest[:0], data...)
		for _, m := range mints {
			if !t.listed[m] {
				t.listed[m] = true
				t.order = append(t.order, m)
			}
		}
		t.mu.Unlock()
		return nil
	})
	return err
}

// saveControl stores non-trending messages, at most one per type every 10 s,
// so an unexpected high-rate topic does not flood the table.
func (t *Trending) saveControl(ctx context.Context, at time.Time, typ string, data []byte) {
	t.mu.Lock()
	last, ok := t.lastSaved[typ]
	if ok && at.Sub(last) < 10*time.Second {
		t.mu.Unlock()
		return
	}
	t.lastSaved[typ] = at
	t.mu.Unlock()
	if !json.Valid(data) {
		data, _ = json.Marshal(map[string]string{"text": string(data)})
	}
	t.rec.dbErr("save ws message", t.db.SaveWSMessage(ctx, at, typ, data))
}

func (t *Trending) sampleLoop(ctx context.Context) {
	tick := time.NewTicker(t.cfg.Sample)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case at := <-tick.C:
			t.mu.Lock()
			raw, order := t.latest, t.order
			t.latest, t.order, t.listed = nil, nil, map[string]bool{}
			t.mu.Unlock()
			if len(order) > 0 {
				t.rec.OnTrending(ctx, at, raw, order)
			}
		}
	}
}
