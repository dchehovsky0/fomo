package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"fomo-thesis-tester/internal/browser"
	"fomo-thesis-tester/internal/fomo"
)

type AccountState string

const (
	Healthy      AccountState = "healthy"
	AuthCooldown AccountState = "auth_cooldown"
	RateLimited  AccountState = "rate_limited"
	Unhealthy    AccountState = "unhealthy"
)

type Account struct {
	ID                   string
	Browser              *browser.Session
	Client               *fomo.Client
	MinInterval          time.Duration
	AuthCooldownDuration time.Duration
	BaseRateCooldown     time.Duration
	MaxRateCooldown      time.Duration
	Log                  *slog.Logger

	mu             sync.Mutex
	state          AccountState
	lastRequest    time.Time
	cooldownUntil  time.Time
	lastSuccess    time.Time
	lastRefresh    time.Time
	accessToken    string
	accessExp      time.Time
	consecutive401 int
	consecutive403 int
	consecutive429 int
}

func NewAccount(id string, b *browser.Session, c *fomo.Client, minInterval, authCooldown, baseRateCooldown, maxRateCooldown time.Duration, log *slog.Logger) *Account {
	return &Account{ID: id, Browser: b, Client: c, MinInterval: minInterval, AuthCooldownDuration: authCooldown, BaseRateCooldown: baseRateCooldown, MaxRateCooldown: maxRateCooldown, Log: log, state: Healthy}
}

func (a *Account) Available(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if now.Before(a.cooldownUntil) {
		return false
	}
	if now.Sub(a.lastRequest) < a.MinInterval {
		return false
	}
	return a.state != Unhealthy
}

func (a *Account) Snapshot() (AccountState, time.Time, time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state, a.cooldownUntil, a.lastSuccess
}

func (a *Account) Do(ctx context.Context, query fomo.ThesisQuery) (fomo.Result, error) {
	if err := a.waitAndMark(ctx); err != nil {
		return fomo.Result{}, err
	}

	token, err := a.ensureToken(ctx, false)
	if err != nil {
		if isContextErr(err) {
			return fomo.Result{}, err
		}
		a.authFailure("token_unavailable", err)
		return fomo.Result{}, err
	}

	res, err := a.Client.GetTheses(ctx, token, query)
	if err == nil {
		a.success()
		return res, nil
	}

	var re *fomo.RequestError
	if !errors.As(err, &re) {
		return res, err
	}

	switch re.Kind {
	case fomo.ErrAuth:
		a.bump401()
		a.Log.Warn("auth_error_recover_once", "account", a.ID, "http_status", re.HTTPStatus, "body_status", re.BodyStatus)
		if recErr := a.Browser.ReloadAndRecover(ctx); recErr != nil {
			a.authFailure("browser_recovery_failed", recErr)
			return res, err
		}
		token, tokErr := a.ensureToken(ctx, true)
		if tokErr != nil {
			a.authFailure("token_recovery_failed", tokErr)
			return res, err
		}
		retry, retryErr := a.Client.GetTheses(ctx, token, query)
		if retryErr == nil {
			a.success()
			return retry, nil
		}
		a.authFailure("auth_retry_failed", retryErr)
		return retry, retryErr

	case fomo.ErrForbidden:
		a.bump403()
		// A 403 may be auth/session context or risk-control. Try exactly one normal
		// page-owned session recovery. If it stays 403, stop using the account for a while.
		a.Log.Warn("forbidden_recover_once", "account", a.ID, "http_status", re.HTTPStatus, "body_status", re.BodyStatus)
		if recErr := a.Browser.ReloadAndRecover(ctx); recErr == nil {
			if token, tokErr := a.ensureToken(ctx, true); tokErr == nil {
				retry, retryErr := a.Client.GetTheses(ctx, token, query)
				if retryErr == nil {
					a.success()
					return retry, nil
				}
				var rre *fomo.RequestError
				if errors.As(retryErr, &rre) && rre.Kind == fomo.ErrForbidden {
					a.authFailure("persistent_403", retryErr)
				}
				return retry, retryErr
			}
		}
		a.authFailure("403_recovery_failed", err)
		return res, err

	case fomo.ErrRateLimit:
		a.rateLimit(re.RetryAfter)
		return res, err
	default:
		return res, err
	}
}

func (a *Account) waitAndMark(ctx context.Context) error {
	for {
		now := time.Now()
		a.mu.Lock()
		wait := a.MinInterval - now.Sub(a.lastRequest)
		if now.Before(a.cooldownUntil) {
			cd := time.Until(a.cooldownUntil)
			if cd > wait {
				wait = cd
			}
		}
		if wait <= 0 && a.state != Unhealthy {
			a.lastRequest = now
			a.mu.Unlock()
			return nil
		}
		if a.state == Unhealthy {
			a.mu.Unlock()
			return errors.New("account unhealthy")
		}
		a.mu.Unlock()
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (a *Account) ensureToken(ctx context.Context, force bool) (string, error) {
	a.mu.Lock()
	token := a.accessToken
	exp := a.accessExp
	a.mu.Unlock()
	if !force && token != "" && time.Until(exp) > 2*time.Minute {
		return token, nil
	}

	token, exp, err := a.Browser.AccessToken(ctx)
	if err != nil {
		if recErr := a.Browser.ReloadAndRecover(ctx); recErr != nil {
			return "", recErr
		}
		token, exp, err = a.Browser.AccessToken(ctx)
		if err != nil {
			return "", err
		}
	}
	a.mu.Lock()
	a.accessToken = token
	a.accessExp = exp
	a.lastRefresh = time.Now()
	a.mu.Unlock()
	return token, nil
}

func (a *Account) success() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state = Healthy
	a.cooldownUntil = time.Time{}
	a.lastSuccess = time.Now()
	a.consecutive401 = 0
	a.consecutive403 = 0
	a.consecutive429 = 0
}

func (a *Account) bump401() { a.mu.Lock(); a.consecutive401++; a.mu.Unlock() }
func (a *Account) bump403() { a.mu.Lock(); a.consecutive403++; a.mu.Unlock() }

func (a *Account) authFailure(reason string, err error) {
	if isContextErr(err) {
		a.Log.Warn("account_context_interrupted", "account", a.ID, "reason", reason, "error", err)
		return
	}
	a.mu.Lock()
	a.state = AuthCooldown
	a.cooldownUntil = time.Now().Add(a.AuthCooldownDuration)
	until := a.cooldownUntil
	a.mu.Unlock()
	a.Log.Error("account_auth_cooldown", "account", a.ID, "reason", reason, "error", err, "until", until.UTC().Format(time.RFC3339Nano))
}

func (a *Account) rateLimit(retryAfter string) {
	a.mu.Lock()
	a.consecutive429++
	n := a.consecutive429
	d := a.BaseRateCooldown
	for i := 1; i < n; i++ {
		d *= 2
		if d >= a.MaxRateCooldown {
			d = a.MaxRateCooldown
			break
		}
	}
	if rd, ok := fomo.ParseRetryAfter(retryAfter, time.Now()); ok && rd > d {
		d = rd
	}
	a.state = RateLimited
	a.cooldownUntil = time.Now().Add(d)
	until := a.cooldownUntil
	a.mu.Unlock()
	a.Log.Warn("account_rate_limited", "account", a.ID, "cooldown", d.String(), "until", until.UTC().Format(time.RFC3339Nano), "retry_after", retryAfter)
}

func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
