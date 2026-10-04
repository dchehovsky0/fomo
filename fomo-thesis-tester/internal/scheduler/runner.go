package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"fomo-thesis-tester/internal/config"
	"fomo-thesis-tester/internal/fomo"
	"fomo-thesis-tester/internal/state"
)

type Runner struct {
	Pool           *Pool
	Tokens         []config.Token
	Tracker        *state.Tracker
	Interval       time.Duration
	RequestTimeout time.Duration
	Lookback       time.Duration
	AuditLookback  time.Duration
	AuditEvery     int
	Limit          int
	Threshold      int
	Log            *slog.Logger

	mu         sync.Mutex
	sequence   int64
	tokenPolls map[string]int64
	stats      *runStats
}

type countStats struct {
	Polls           int64
	OK              int64
	Failed          int64
	Alerts          int64
	WindowSaturated int64
	LastStart       time.Time
	MaxStartGap     time.Duration
}

type runStats struct {
	StartedAt time.Time
	Total     countStats
	Accounts  map[string]*countStats
	Tokens    map[string]*countStats
}

func (r *Runner) Run(ctx context.Context) error {
	if len(r.Tokens) == 0 {
		return errors.New("no tokens")
	}
	if r.Tracker == nil {
		return errors.New("tracker is nil")
	}
	if r.Limit <= 0 {
		r.Limit = 500
	}
	if r.Lookback <= 0 {
		r.Lookback = 5 * time.Minute
	}
	if r.AuditLookback <= 0 {
		r.AuditLookback = 30 * time.Minute
	}
	if r.tokenPolls == nil {
		r.tokenPolls = make(map[string]int64)
	}
	r.stats = &runStats{
		StartedAt: time.Now().UTC(),
		Accounts:  make(map[string]*countStats),
		Tokens:    make(map[string]*countStats),
	}

	r.Log.Info("scheduler_started",
		"tokens", len(r.Tokens),
		"accounts", len(r.Pool.Accounts),
		"interval", r.Interval.String(),
		"mode", "round_robin",
		"endpoint", "/feed/token/sortedThesis",
		"limit", r.Limit,
		"threshold", r.Threshold,
		"live_lookback", r.Lookback.String(),
		"audit_lookback", r.AuditLookback.String(),
		"audit_every_token_polls", r.AuditEvery,
		"expected_token_poll_interval", (time.Duration(len(r.Tokens)) * r.Interval).String(),
	)
	defer r.logSummary()

	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()
	nextToken := 0

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case scheduledAt := <-ticker.C:
			tokenIndex := nextToken
			token := r.Tokens[tokenIndex]
			nextToken = (nextToken + 1) % len(r.Tokens)

			acc, err := r.Pool.Pick(time.Now())
			if err != nil {
				r.Log.Warn("scheduler_no_account",
					"scheduled_at", scheduledAt.UTC().Format(time.RFC3339Nano),
					"token_index", tokenIndex,
					"address", token.Address,
					"network_id", token.NetworkID,
					"error", err,
				)
				continue
			}

			query, seq, tokenPoll := r.buildQuery(token)
			dispatchedAt := time.Now().UTC()
			r.notePollStarted(acc.ID, token, dispatchedAt)
			r.Log.Debug("poll_started",
				"sequence", seq,
				"token_poll_number", tokenPoll,
				"scheduled_at", scheduledAt.UTC().Format(time.RFC3339Nano),
				"dispatched_at", dispatchedAt.Format(time.RFC3339Nano),
				"schedule_lag_ms", dispatchedAt.Sub(scheduledAt.UTC()).Milliseconds(),
				"account", acc.ID,
				"token_index", tokenIndex,
				"address", token.Address,
				"network_id", token.NetworkID,
				"query_mode", query.Mode,
				"query_after", query.AfterTime.Format(time.RFC3339Nano),
				"query_before", query.BeforeTime.Format(time.RFC3339Nano),
				"limit", query.Limit,
				"threshold", query.Threshold,
			)

			reqCtx, cancel := context.WithTimeout(ctx, r.RequestTimeout)
			res, err := acc.Do(reqCtx, query)
			cancel()
			r.handle(acc.ID, token, seq, tokenPoll, query, res, err)
		}
	}
}

func (r *Runner) buildQuery(token config.Token) (fomo.ThesisQuery, int64, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sequence++
	seq := r.sequence
	k := tokenStatKey(token)
	r.tokenPolls[k]++
	pollN := r.tokenPolls[k]

	mode := "live"
	lookback := r.Lookback
	if r.AuditEvery > 0 && pollN%int64(r.AuditEvery) == 0 {
		mode = "audit"
		lookback = r.AuditLookback
	}
	before := time.Now().UTC()
	return fomo.ThesisQuery{
		Address:    token.Address,
		NetworkID:  token.NetworkID,
		AfterTime:  before.Add(-lookback),
		BeforeTime: before,
		Limit:      r.Limit,
		Threshold:  r.Threshold,
		Mode:       mode,
	}, seq, pollN
}

func (r *Runner) handle(accountID string, token config.Token, seq, tokenPoll int64, query fomo.ThesisQuery, res fomo.Result, err error) {
	if err != nil {
		r.notePollResult(accountID, token, false, false, 0)
		attrs := []any{
			"sequence", seq,
			"token_poll_number", tokenPoll,
			"account", accountID,
			"address", token.Address,
			"network_id", token.NetworkID,
			"query_mode", query.Mode,
			"query_after", query.AfterTime.Format(time.RFC3339Nano),
			"query_before", query.BeforeTime.Format(time.RFC3339Nano),
			"go_request_started_at", formatTime(res.RequestStartedAt),
			"browser_fetch_started_at", formatTime(res.BrowserFetchStartedAt),
			"browser_headers_received_at", formatTime(res.BrowserHeadersReceivedAt),
			"browser_body_read_at", formatTime(res.BrowserBodyReadAt),
			"failed_at", time.Now().UTC().Format(time.RFC3339Nano),
			"latency_ms", res.Latency.Milliseconds(),
		}
		var re *fomo.RequestError
		if errors.As(err, &re) {
			attrs = append(attrs,
				"kind", re.Kind,
				"http_status", re.HTTPStatus,
				"body_status", re.BodyStatus,
				"retry_after", re.RetryAfter,
				"error", re.Error(),
			)
		} else {
			attrs = append(attrs, "error", err)
		}
		r.Log.Warn("poll_failed", attrs...)
		return
	}

	items := res.Feed.ResponseObject.Items
	actualRequestStart := effectiveRequestStart(res)
	complete := !res.Feed.ResponseObject.HasNextPage
	limitHit := len(items) >= query.Limit
	meta := state.PollMeta{
		AccountID:              accountID,
		Sequence:               seq,
		TokenPollNumber:        tokenPoll,
		QueryMode:              query.Mode,
		RequestStarted:         actualRequestStart,
		BrowserHeadersReceived: res.BrowserHeadersReceivedAt,
		BrowserBodyRead:        res.BrowserBodyReadAt,
		ReceivedAt:             res.ReceivedAt,
		QueryAfter:             query.AfterTime,
		QueryBefore:            query.BeforeTime,
		ServerDate:             res.ServerDate,
		Complete:               complete,
		HasNextPage:            res.Feed.ResponseObject.HasNextPage,
		ItemCount:              len(items),
		Limit:                  query.Limit,
	}
	alerts, baseline := r.Tracker.Observe(token.Address, token.NetworkID, meta, items)
	r.notePollResult(accountID, token, true, !complete || limitHit, int64(len(alerts)))

	r.Log.Info("poll_ok",
		"sequence", seq,
		"token_poll_number", tokenPoll,
		"account", accountID,
		"address", token.Address,
		"network_id", token.NetworkID,
		"query_mode", query.Mode,
		"query_after", query.AfterTime.Format(time.RFC3339Nano),
		"query_before", query.BeforeTime.Format(time.RFC3339Nano),
		"go_request_started_at", res.RequestStartedAt.Format(time.RFC3339Nano),
		"browser_fetch_started_at", formatTime(res.BrowserFetchStartedAt),
		"browser_headers_received_at", formatTime(res.BrowserHeadersReceivedAt),
		"browser_body_read_at", formatTime(res.BrowserBodyReadAt),
		"received_at", res.ReceivedAt.Format(time.RFC3339Nano),
		"server_date", res.ServerDate,
		"network_to_headers_ms", diffMSOrNil(res.BrowserFetchStartedAt, res.BrowserHeadersReceivedAt),
		"body_read_ms", diffMSOrNil(res.BrowserHeadersReceivedAt, res.BrowserBodyReadAt),
		"cdp_return_ms", diffMSOrNil(res.BrowserBodyReadAt, res.ReceivedAt),
		"http_status", res.HTTPStatus,
		"body_status", res.Feed.StatusCode,
		"items", len(items),
		"response_count", res.Feed.ResponseObject.Count,
		"has_next_page", res.Feed.ResponseObject.HasNextPage,
		"window_complete", complete,
		"limit_hit", limitHit,
		"baseline", baseline,
		"new_unique_theses", len(alerts),
		"latency_ms", res.Latency.Milliseconds(),
	)

	if !complete || limitHit {
		r.Log.Warn("query_window_saturated",
			"sequence", seq,
			"account", accountID,
			"address", token.Address,
			"network_id", token.NetworkID,
			"query_mode", query.Mode,
			"items", len(items),
			"limit", query.Limit,
			"has_next_page", res.Feed.ResponseObject.HasNextPage,
			"response_count", res.Feed.ResponseObject.Count,
			"query_after", query.AfterTime.Format(time.RFC3339Nano),
			"query_before", query.BeforeTime.Format(time.RFC3339Nano),
		)
	}

	for _, alert := range alerts {
		th := alert.Thesis
		outsideLiveWindow := alert.CreatedAt.Before(res.ReceivedAt.Add(-r.Lookback))
		attrs := []any{
			"canonical_id", alert.CanonicalID,
			"received_at", alert.FirstSeenReceivedAt.Format(time.RFC3339Nano),
			"created_at_backend", th.CreatedAt,
			"created_at_backend_utc", alert.CreatedAt.Format(time.RFC3339Nano),
			"first_seen_request_started_at", alert.FirstSeenRequestStartedAt.Format(time.RFC3339Nano),
			"first_seen_browser_headers_received_at", formatTime(alert.FirstSeenBrowserHeadersAt),
			"first_seen_browser_body_read_at", formatTime(alert.FirstSeenBrowserBodyReadAt),
			"go_received_at", alert.FirstSeenReceivedAt.Format(time.RFC3339Nano),
			"detection_delay_ms", alert.DetectionDelay.Milliseconds(),
			"backend_returned_upper_bound_ms", alert.BackendReturnedUpperBound.Milliseconds(),
			"fetch_duration_ms", alert.FetchDuration.Milliseconds(),
			"first_poll_after_created_at_started_at", formatTime(alert.FirstEligiblePollStartedAt),
			"first_poll_after_created_at_received_at", formatTime(alert.FirstEligiblePollReceivedAt),
			"first_poll_wait_ms", durationMSOrNil(alert.FirstEligiblePollStartedAt, alert.FirstPollWait),
			"eligible_successful_polls_before_seen", alert.EligiblePollsBeforeSeen,
			"backend_lag_upper_bound_ms", alert.BackendLagUpperBound.Milliseconds(),
			"availability_bound_known", alert.HasAvailabilityBound,
			"latency_confidence", alert.LatencyConfidence,
			"current_window_complete", alert.CurrentWindowComplete,
			"outside_live_lookback_at_detection", outsideLiveWindow,
			"query_mode", query.Mode,
			"server_date", res.ServerDate,
			"address", token.Address,
			"network_id", token.NetworkID,
			"thesis_id", th.ID,
			"comment_id", th.Comment.ID,
			"trade_id", firstNonEmpty(th.TradeID, th.Comment.TradeID),
			"user_id", th.UserID,
			"author", th.DisplayName,
			"handle", th.UserHandle,
			"comment", th.Comment.Comment,
			"account", accountID,
		}
		if alert.HasAvailabilityBound {
			attrs = append(attrs,
				"last_absent_complete_poll_received_at", alert.LastAbsentPollReceivedAt.Format(time.RFC3339Nano),
				"backend_lag_lower_bound_ms", alert.BackendLagLowerBound.Milliseconds(),
				"backend_visibility_window_ms", alert.BackendVisibilityWindow.Milliseconds(),
			)
		}
		r.Log.Warn("NEW_THESIS", attrs...)
	}
}

func (r *Runner) notePollStarted(accountID string, token config.Token, started time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stats == nil {
		return
	}
	r.stats.Total.Polls++
	as := ensureStats(r.stats.Accounts, accountID)
	as.Polls++
	ts := ensureStats(r.stats.Tokens, tokenStatKey(token))
	ts.Polls++
	if !ts.LastStart.IsZero() {
		gap := started.Sub(ts.LastStart)
		if gap > ts.MaxStartGap {
			ts.MaxStartGap = gap
		}
	}
	ts.LastStart = started
}

func (r *Runner) notePollResult(accountID string, token config.Token, ok, saturated bool, alerts int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stats == nil {
		return
	}
	as := ensureStats(r.stats.Accounts, accountID)
	ts := ensureStats(r.stats.Tokens, tokenStatKey(token))
	if ok {
		r.stats.Total.OK++
		as.OK++
		ts.OK++
	} else {
		r.stats.Total.Failed++
		as.Failed++
		ts.Failed++
	}
	if saturated {
		r.stats.Total.WindowSaturated++
		as.WindowSaturated++
		ts.WindowSaturated++
	}
	r.stats.Total.Alerts += alerts
	as.Alerts += alerts
	ts.Alerts += alerts
}

func (r *Runner) logSummary() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stats == nil {
		return
	}
	r.Log.Info("run_summary",
		"started_at", r.stats.StartedAt.Format(time.RFC3339Nano),
		"finished_at", time.Now().UTC().Format(time.RFC3339Nano),
		"polls", r.stats.Total.Polls,
		"poll_ok", r.stats.Total.OK,
		"poll_failed", r.stats.Total.Failed,
		"new_unique_theses", r.stats.Total.Alerts,
		"saturated_windows", r.stats.Total.WindowSaturated,
	)

	accounts := make([]string, 0, len(r.stats.Accounts))
	for id := range r.stats.Accounts {
		accounts = append(accounts, id)
	}
	sort.Strings(accounts)
	for _, id := range accounts {
		s := r.stats.Accounts[id]
		r.Log.Info("account_run_summary", "account", id, "polls", s.Polls, "poll_ok", s.OK, "poll_failed", s.Failed, "new_unique_theses", s.Alerts, "saturated_windows", s.WindowSaturated)
	}

	tokens := make([]string, 0, len(r.stats.Tokens))
	for k := range r.stats.Tokens {
		tokens = append(tokens, k)
	}
	sort.Strings(tokens)
	for _, k := range tokens {
		s := r.stats.Tokens[k]
		r.Log.Info("token_run_summary", "token", k, "polls", s.Polls, "poll_ok", s.OK, "poll_failed", s.Failed, "new_unique_theses", s.Alerts, "saturated_windows", s.WindowSaturated, "max_poll_start_gap_ms", s.MaxStartGap.Milliseconds(), "last_poll_start_at", formatTime(s.LastStart))
	}
}

func ensureStats(m map[string]*countStats, key string) *countStats {
	if s := m[key]; s != nil {
		return s
	}
	s := &countStats{}
	m[key] = s
	return s
}

func tokenStatKey(t config.Token) string {
	return fmt.Sprintf("%d|%s", t.NetworkID, t.Address)
}

func formatTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func durationMSOrNil(t time.Time, d time.Duration) any {
	if t.IsZero() {
		return nil
	}
	return d.Milliseconds()
}

func effectiveRequestStart(res fomo.Result) time.Time {
	if !res.BrowserFetchStartedAt.IsZero() {
		return res.BrowserFetchStartedAt
	}
	return res.RequestStartedAt
}

func diffMSOrNil(start, end time.Time) any {
	if start.IsZero() || end.IsZero() {
		return nil
	}
	return end.Sub(start).Milliseconds()
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
