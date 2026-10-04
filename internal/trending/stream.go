// Package trending follows fomo's trending_tokens WebSocket topic and alerts
// when a token shows up in the trending list for the first time.
package trending

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"fomobot/internal/fomo"
	"fomobot/internal/session"
)

const (
	KindSnapshot = "snapshot" // the whole list, in trending order
	KindUpdate   = "update"   // one entry changed; Index is its position
	KindRemove   = "remove"   // a token left the list
)

type Token struct {
	Mint      string
	NetworkID string
	Symbol    string
	Name      string
	Launchpad string
	ImageURL  string
	// Rank is the 1-based position in fomo's trending list (all chains).
	Rank      int
	MarketCap float64
	PriceUSD  float64
	// Change24 is a fraction: 0.5 means +50%.
	Change24 float64
	Volume24 float64
	Holders  int
}

type Message struct {
	Kind   string
	Tokens []Token
	// Removed is the mint of a KindRemove message.
	Removed        string
	RemovedNetwork string
}

type wsItem struct {
	Change24  fomo.FlexFloat `json:"change24"`
	Holders   fomo.FlexInt   `json:"holders"`
	MarketCap fomo.FlexFloat `json:"marketCap"`
	PriceUSD  fomo.FlexFloat `json:"priceUSD"`
	Volume24  fomo.FlexFloat `json:"volume24"`
	Token     struct {
		Address   string          `json:"address"`
		Name      string          `json:"name"`
		Symbol    string          `json:"symbol"`
		NetworkID fomo.FlexString `json:"networkId"`
		Info      *struct {
			Image string `json:"imageThumbUrl"`
		} `json:"info"`
		Launchpad *struct {
			Name string `json:"launchpadName"`
		} `json:"launchpad"`
	} `json:"token"`
}

func (it wsItem) token(rank int) Token {
	t := Token{
		Mint: it.Token.Address, NetworkID: it.Token.NetworkID.String(),
		Symbol: it.Token.Symbol, Name: it.Token.Name, Rank: rank,
		MarketCap: it.MarketCap.Float(), PriceUSD: it.PriceUSD.Float(),
		Change24: it.Change24.Float(), Volume24: it.Volume24.Float(), Holders: int(it.Holders),
	}
	if it.Token.Launchpad != nil {
		t.Launchpad = it.Token.Launchpad.Name
	}
	if it.Token.Info != nil {
		t.ImageURL = it.Token.Info.Image
	}
	return t
}

type wsMessage struct {
	Type      string `json:"type"`
	TopicType string `json:"topicType"`
	Payload   struct {
		Kind     string   `json:"kind"`
		Tokens   []wsItem `json:"tokens"`
		Index    *int     `json:"index"`
		TokenKey string   `json:"tokenKey"`
		Update   *wsItem  `json:"update"`
	} `json:"payload"`
}

// Parse decodes a trending_tokens data message. ok is false for anything else
// (control messages, other topics, unknown kinds).
func Parse(data []byte) (Message, bool) {
	var m wsMessage
	if err := json.Unmarshal(data, &m); err != nil || m.Type != "data" {
		return Message{}, false
	}
	if m.TopicType != "" && m.TopicType != "trending_tokens" {
		return Message{}, false
	}
	p := m.Payload
	switch p.Kind {
	case KindSnapshot:
		msg := Message{Kind: KindSnapshot, Tokens: make([]Token, 0, len(p.Tokens))}
		for i, it := range p.Tokens {
			if it.Token.Address != "" {
				msg.Tokens = append(msg.Tokens, it.token(i+1))
			}
		}
		return msg, true
	case KindUpdate:
		if p.Update == nil || p.Update.Token.Address == "" {
			return Message{}, false
		}
		rank := 0
		if p.Index != nil {
			rank = *p.Index + 1
		}
		return Message{Kind: KindUpdate, Tokens: []Token{p.Update.token(rank)}}, true
	case KindRemove:
		mint, network, _ := strings.Cut(p.TokenKey, ":")
		if mint == "" {
			return Message{}, false
		}
		return Message{Kind: KindRemove, Removed: mint, RemovedNetwork: network}, true
	}
	return Message{}, false
}

type StreamConfig struct {
	URL       string
	TopicID   string
	Origin    string
	UserAgent string
}

type StreamStats struct {
	Messages   atomic.Int64
	Reconnects atomic.Int64
	// LastMessage is the unix ms of the last trending message.
	LastMessage atomic.Int64
}

// Stream keeps a subscription to trending_tokens alive
// (challenge -> challengeResponse{jwt} -> challengeAccepted -> subscribe).
type Stream struct {
	cfg    StreamConfig
	tokens session.TokenSource
	log    *slog.Logger
	Stats  StreamStats
}

func NewStream(cfg StreamConfig, tokens session.TokenSource, log *slog.Logger) *Stream {
	return &Stream{cfg: cfg, tokens: tokens, log: log}
}

// Run calls handle for every trending message until ctx is done, reconnecting
// with backoff. handle runs on the reading goroutine and must be quick.
func (s *Stream) Run(ctx context.Context, handle func(Message)) {
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := s.session(ctx, handle)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > 2*time.Minute {
			backoff = 5 * time.Second
		}
		s.Stats.Reconnects.Add(1)
		s.log.Warn("trending websocket disconnected", "err", err,
			"up_for", time.Since(start).Round(time.Second), "retry_in", backoff)
		if sleep(ctx, backoff) != nil {
			return
		}
		backoff = min(backoff*2, 5*time.Minute)
	}
}

func (s *Stream) session(ctx context.Context, handle func(Message)) error {
	// fomo sends updates every 1-2 s; silence means a dead connection.
	return fomo.WS{
		URL: s.cfg.URL, Origin: s.cfg.Origin, UserAgent: s.cfg.UserAgent,
		TopicType: "trending_tokens", TopicID: s.cfg.TopicID, Idle: 2 * time.Minute,
		Tokens: s.tokens, Log: s.log,
	}.Run(ctx, func(_ string, data []byte) error {
		if msg, ok := Parse(data); ok {
			s.Stats.Messages.Add(1)
			s.Stats.LastMessage.Store(time.Now().UnixMilli())
			handle(msg)
		}
		return nil
	})
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
