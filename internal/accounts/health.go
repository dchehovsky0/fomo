// Package accounts tracks whether each fomo account works: from the requests
// the bot makes anyway, and from a cheap probe of accounts that sat idle.
package accounts

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	"fomobot/internal/fomo"
	"fomobot/internal/session"
)

type State string

const (
	StateOK          State = "ok"
	StateUnknown     State = "unknown"
	StateNeedsLogin  State = "needs_login"
	StateRejected    State = "rejected"
	StateRateLimited State = "rate_limited"
	StateFailing     State = "failing"
)

const (
	// failingAfter consecutive network or 5xx errors mark an account failing.
	failingAfter = 3
	// recentOK: a rate-limited account still counts as working if it had a
	// successful request this recently.
	recentOK = 5 * time.Minute
)

type Account struct {
	name  string
	proxy string
	rps   float64
	probe func(ctx context.Context) error

	mu sync.Mutex
	// seq orders events; clock readings can tie on Windows.
	seq                             uint64
	okSeq, authSeq, rlSeq, tokenSeq uint64

	requests     int64
	errors       int64
	rateLimited  int64
	authFailures int64
	consecutive  int
	lastOK       time.Time
	lastErrAt    time.Time
	lastErr      string
	tokenErr     error
	tokenErrAt   time.Time
	tokenExp     time.Time
	lastProbe    time.Time
}

func (a *Account) Name() string { return a.name }

// SetProbe sets the cheap request used to check an idle account. Call it
// before the registry starts.
func (a *Account) SetProbe(probe func(ctx context.Context) error) { a.probe = probe }

// ObserveCall is the fomo client's OnCall hook: one HTTP attempt.
func (a *Account) ObserveCall(ci fomo.CallInfo) {
	at := ci.At.Add(ci.Duration)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests++
	a.seq++
	switch {
	case ci.Err != nil:
		if errors.Is(ci.Err, context.Canceled) {
			return
		}
		a.fail(at, ci.Err.Error())
	case ci.Status >= 200 && ci.Status < 400:
		a.lastOK, a.consecutive, a.okSeq = at, 0, a.seq
	case ci.Status == 401 || ci.Status == 430 || ci.Status == 431:
		a.authFailures++
		a.authSeq = a.seq
	case ci.Status == 429:
		a.rateLimited++
		a.rlSeq = a.seq
	case ci.Status >= 500:
		a.fail(at, fmt.Sprintf("HTTP %d", ci.Status))
	}
}

func (a *Account) fail(at time.Time, msg string) {
	a.errors++
	a.consecutive++
	a.lastErrAt, a.lastErr = at, msg
}

func (a *Account) observeToken(tok string, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seq++
	if err != nil {
		a.tokenErr, a.tokenErrAt, a.tokenSeq = err, time.Now(), a.seq
		return
	}
	a.tokenErr = nil
	if exp, err := session.ParseExpiry(tok); err == nil {
		a.tokenExp = exp
	}
}

type Status struct {
	Name              string    `json:"name"`
	State             State     `json:"state"`
	Healthy           bool      `json:"healthy"`
	Reason            string    `json:"reason,omitempty"`
	Proxy             string    `json:"proxy,omitempty"`
	TokenExpiresAt    time.Time `json:"token_expires_at,omitzero"`
	LastOKAt          time.Time `json:"last_ok_at,omitzero"`
	LastErrorAt       time.Time `json:"last_error_at,omitzero"`
	LastError         string    `json:"last_error,omitempty"`
	ConsecutiveErrors int       `json:"consecutive_errors,omitempty"`
	Requests          int64     `json:"requests"`
	Errors            int64     `json:"errors"`
	RateLimited       int64     `json:"rate_limited"`
	AuthFailures      int64     `json:"auth_failures"`
}

func (a *Account) Status(now time.Time) Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := Status{
		Name: a.name, Proxy: a.proxy, TokenExpiresAt: a.tokenExp, LastOKAt: a.lastOK,
		LastErrorAt: a.lastErrAt, LastError: a.lastErr, ConsecutiveErrors: a.consecutive,
		Requests: a.requests, Errors: a.errors, RateLimited: a.rateLimited, AuthFailures: a.authFailures,
	}
	login := "fomobot -login -account " + a.name
	switch {
	case a.tokenErr != nil && a.tokenSeq > a.okSeq:
		s.LastError, s.LastErrorAt = a.tokenErr.Error(), a.tokenErrAt
		if errors.Is(a.tokenErr, session.ErrNoToken) {
			s.State, s.Reason = StateNeedsLogin, "нет входа в fomo в профиле Chrome: "+login
		} else {
			s.State, s.Reason = StateFailing, "не удалось получить токен fomo"
		}
	case a.authSeq > a.okSeq:
		s.State, s.Reason = StateRejected, "fomo отклоняет токен: войди заново ("+login+") или аккаунт заблокирован"
	case a.consecutive >= failingAfter:
		s.State, s.Reason = StateFailing, "запросы не проходят"
		if a.proxy != "" {
			s.Reason += ", проверь прокси"
		}
	case a.rlSeq > a.okSeq:
		s.State, s.Reason = StateRateLimited, "fomo ограничивает частоту запросов (429)"
		s.Healthy = !a.lastOK.IsZero() && now.Sub(a.lastOK) < recentOK
		return s
	case a.lastOK.IsZero():
		s.State, s.Reason = StateUnknown, "ещё не проверялся"
	default:
		s.State = StateOK
	}
	s.Healthy = s.State == StateOK
	return s
}

// idle reports whether the account needs a probe: no success and no probe
// within d.
func (a *Account) idle(now time.Time, d time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.probe != nil && now.Sub(a.lastOK) >= d && now.Sub(a.lastProbe) >= d
}

func (a *Account) runProbe(ctx context.Context) error {
	a.mu.Lock()
	a.lastProbe = time.Now()
	a.mu.Unlock()
	return a.probe(ctx)
}

// tracked records every token the account's session hands out, or fails to.
type tracked struct {
	session.TokenSource
	a *Account
}

func (t tracked) Token(ctx context.Context) (string, error) {
	tok, err := t.TokenSource.Token(ctx)
	t.a.observeToken(tok, err)
	return tok, err
}

// Track wraps the account's token source so the health shows login problems.
func (a *Account) Track(src session.TokenSource) session.TokenSource {
	return tracked{TokenSource: src, a: a}
}

// maskProxy drops credentials: the status is shown over HTTP.
func maskProxy(proxy string) string {
	if proxy == "" {
		return ""
	}
	u, err := url.Parse(proxy)
	if err != nil || u.Host == "" {
		return "(задан)"
	}
	return u.Scheme + "://" + u.Host
}
