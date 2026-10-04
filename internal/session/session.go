package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// TokenSource hands out a Privy access token (JWT) accepted by prod-api.fomo.family.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	// Invalidate is called when the API rejects the token (401/431).
	Invalidate()
}

var ErrNoToken = errors.New("no fomo access token")

// Failover hands out a token of any account that has one, starting with the
// one that worked last, for consumers that do not care which account it is.
type Failover struct {
	srcs []TokenSource
	mu   sync.Mutex
	cur  int
}

func NewFailover(srcs ...TokenSource) *Failover {
	return &Failover{srcs: srcs}
}

func (f *Failover) Token(ctx context.Context) (string, error) {
	if len(f.srcs) == 0 {
		return "", ErrNoToken
	}
	f.mu.Lock()
	start := f.cur
	f.mu.Unlock()
	var last error
	for i := range f.srcs {
		idx := (start + i) % len(f.srcs)
		tok, err := f.srcs[idx].Token(ctx)
		if err == nil {
			f.mu.Lock()
			f.cur = idx
			f.mu.Unlock()
			return tok, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		last = err
	}
	return "", last
}

// Invalidate passes the rejection to the current account and moves on to the next one.
func (f *Failover) Invalidate() {
	if len(f.srcs) == 0 {
		return
	}
	f.mu.Lock()
	src := f.srcs[f.cur]
	f.cur = (f.cur + 1) % len(f.srcs)
	f.mu.Unlock()
	src.Invalidate()
}

// Normalize accepts the raw value of localStorage["privy:token"] (a JSON string
// in quotes), a "Bearer ..." header value or a bare JWT.
func Normalize(raw string) string {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, `"`) {
		var v string
		if json.Unmarshal([]byte(s), &v) == nil {
			s = v
		}
	}
	s = strings.TrimSpace(s)
	if len(s) > 7 && strings.EqualFold(s[:7], "bearer ") {
		s = s[7:]
	}
	return strings.TrimSpace(s)
}

// ParseExpiry reads the exp claim without verifying the signature.
func ParseExpiry(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, errors.New("not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, fmt.Errorf("decode JWT payload: %w", err)
	}
	var claims struct {
		Exp float64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, fmt.Errorf("parse JWT payload: %w", err)
	}
	if claims.Exp == 0 {
		return time.Time{}, errors.New("JWT has no exp claim")
	}
	return time.Unix(int64(claims.Exp), 0), nil
}
