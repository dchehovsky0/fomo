// Package recorder writes everything seen on fomo to PostgreSQL: every thesis
// from the live feed, the thesis history and count of every token that shows
// up, token metrics over time and the trending list.
package recorder

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"fomobot/internal/db"
	"fomobot/internal/fomo"
)

// A thesis created up to this long before the start still counts as created
// while running: the first feed request itself takes time.
const startGrace = 2 * time.Minute

type API interface {
	FeedWithStatus(ctx context.Context, limit int) ([]fomo.Event, bool, error)
	TokenThesis(ctx context.Context, mint, lastID string) (*fomo.TokenThesisPage, error)
	TokenDetails(ctx context.Context, mint string) (json.RawMessage, error)
	FilterTokens(ctx context.Context, mints []string) (json.RawMessage, error)
}

type Config struct {
	NetworkID          string
	FeedLimit          int
	FeedInterval       time.Duration
	HistoryMaxPages    int
	RefreshMinInterval time.Duration
	NewTokenRefresh    time.Duration
	NewTokenTrackFor   time.Duration
	AllTokensRefresh   time.Duration
	TrackFor           time.Duration
	SnapshotInterval   time.Duration
	SnapshotBatch      int
	DetailsInterval    time.Duration
	TrendingPages      int
	RetryDelay         time.Duration
}

const (
	SourceStartupFeed = "startup_feed"
	SourceFeed        = "feed"
	SourceTrending    = "trending"
)

type tokenState struct {
	source       string
	firstSeen    time.Time
	lastActivity time.Time
	complete     bool
	firstThesis  time.Time
	count        int
	lastRefresh  time.Time
	detailsAt    time.Time
	announced    bool
}

type Stats struct {
	FeedPolls     atomic.Int64
	FeedChanged   atomic.Int64
	NewTheses     atomic.Int64
	Observations  atomic.Int64
	TokenChecks   atomic.Int64
	Snapshots     atomic.Int64
	Details       atomic.Int64
	TrendingSaved atomic.Int64
	DBErrors      atomic.Int64
	APIErrors     atomic.Int64
}

type Recorder struct {
	cfg   Config
	api   API
	db    *db.DB
	log   *slog.Logger
	now   func() time.Time
	runID int64
	Stats Stats

	startedAt        time.Time
	queue            *queue
	snapshotRawSaved atomic.Bool

	mu         sync.Mutex
	tokens     map[string]*tokenState
	seenEvents map[string]time.Time
	obsHash    map[string]uint64 // source|thesis id -> state hash
}

func New(cfg Config, api API, store *db.DB, runID int64, log *slog.Logger) *Recorder {
	if cfg.RetryDelay <= 0 {
		cfg.RetryDelay = 30 * time.Second
	}
	return &Recorder{
		cfg:        cfg,
		api:        api,
		db:         store,
		log:        log,
		now:        time.Now,
		runID:      runID,
		startedAt:  time.Now(),
		queue:      newQueue(),
		tokens:     map[string]*tokenState{},
		seenEvents: map[string]time.Time{},
		obsHash:    map[string]uint64{},
	}
}

// Restore loads recently active tokens so a restart keeps following them.
func (r *Recorder) Restore(ctx context.Context) error {
	known, err := r.db.LoadTokens(ctx, r.now().Add(-r.cfg.TrackFor))
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for _, k := range known {
		r.tokens[k.Address] = &tokenState{
			source:       "restored",
			firstSeen:    k.FirstSeenAt,
			lastActivity: k.LastActivityAt,
			complete:     k.HistoryComplete,
			firstThesis:  k.FirstThesisAt,
			count:        k.Count,
			lastRefresh:  k.CountCheckedAt,
			detailsAt:    k.DetailsAt,
			announced:    true,
		}
		if k.CountCheckedAt.IsZero() {
			r.queue.push(job{kind: jobHistory, mint: k.Address, prio: prioBackfill, pages: r.cfg.HistoryMaxPages, due: now})
		}
	}
	if len(known) > 0 {
		r.log.Info("restored tokens from database", "tokens", len(known))
	}
	return nil
}

// Run blocks until ctx is done. Trending messages come in through OnTrending.
func (r *Recorder) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { r.feedLoop(ctx) })
	wg.Go(func() { r.worker(ctx) })
	wg.Go(func() { r.scheduleLoop(ctx) })
	wg.Go(func() { r.snapshotLoop(ctx) })
	wg.Wait()
}

func (r *Recorder) QueueLen() int { return r.queue.len() }

func (r *Recorder) Tracked() (all, young int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for _, t := range r.tokens {
		if r.active(t, now) {
			all++
			if r.young(t, now) {
				young++
			}
		}
	}
	return all, young
}

// ---- feed ----

func (r *Recorder) feedLoop(ctx context.Context) {
	initial := true
	for {
		events, notModified, err := r.api.FeedWithStatus(ctx, r.cfg.FeedLimit)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			r.Stats.APIErrors.Add(1)
			r.log.Warn("feed poll failed", "err", err)
		default:
			r.Stats.FeedPolls.Add(1)
			if !notModified {
				r.Stats.FeedChanged.Add(1)
				r.handleFeed(ctx, events, initial)
			}
			initial = false
		}
		if sleep(ctx, r.cfg.FeedInterval) != nil {
			return
		}
	}
}

func (r *Recorder) handleFeed(ctx context.Context, events []fomo.Event, initial bool) {
	now := r.now()
	var fresh []db.FeedEvent
	var theses []db.Thesis
	type sighting struct {
		mint, ticker, image string
		live                bool
	}
	var sightings []sighting

	r.mu.Lock()
	for i := range events {
		e := &events[i]
		id := e.ID.String()
		_, seen := r.seenEvents[id]
		if !seen && id != "" {
			r.seenEvents[id] = now
			fresh = append(fresh, db.FeedEvent{
				ID: id, Type: e.Type, TokenAddress: e.TokenAddress, NetworkID: e.NetworkID.String(),
				ThesisID: e.ThesisID(), CreatedAt: e.CreatedAt.Time, SeenAt: now, Raw: e.Raw,
			})
		}
		if !e.IsThesis() || e.NetworkID.String() != r.cfg.NetworkID || e.ThesisID() == "" {
			continue
		}
		th := feedThesis(e, now)
		if r.changed(&th) {
			theses = append(theses, th)
		}
		if !seen {
			// A thesis is live when it was created after the start; pinned or
			// late-indexed old theses are recorded but not treated as activity.
			live := !initial && !th.CreatedAt.Before(r.startedAt.Add(-startGrace))
			sightings = append(sightings, sighting{e.TokenAddress, e.Body.Ticker, e.Body.TokenImageURL, live})
		}
	}
	r.pruneSeenLocked(now)
	r.mu.Unlock()

	r.dbErr("save feed events", r.db.SaveFeedEvents(ctx, fresh))
	r.dbErr("save theses", r.db.SaveTheses(ctx, theses))
	r.Stats.Observations.Add(int64(len(theses)))

	for _, s := range sightings {
		source := SourceFeed
		if initial {
			source = SourceStartupFeed
		}
		isNew := r.noteToken(ctx, s.mint, s.ticker, s.image, source, now, s.live)
		if s.live {
			r.Stats.NewTheses.Add(1)
			r.dbErr("mark feed thesis", r.db.MarkFeedThesis(ctx, s.mint, now))
		}
		switch {
		case isNew && initial:
			r.queue.push(job{kind: jobHistory, mint: s.mint, prio: prioBackfill, pages: r.cfg.HistoryMaxPages, due: now})
		case isNew:
			r.log.Info("new token in feed", "token", s.mint, "ticker", s.ticker, "live", s.live)
			r.queue.push(job{kind: jobHistory, mint: s.mint, prio: prioUrgent, pages: r.cfg.HistoryMaxPages, due: now})
		case s.live:
			r.scheduleRefresh(s.mint, prioUrgent, now, "new thesis")
		}
	}
}

// changed reports whether the thesis differs from its previous observation of
// the same source, and remembers it.
func (r *Recorder) changed(t *db.Thesis) bool {
	key := t.Source + "|" + t.ID
	h := stateHash(t)
	if prev, ok := r.obsHash[key]; ok && prev == h {
		return false
	}
	r.obsHash[key] = h
	return true
}

func (r *Recorder) pruneSeenLocked(now time.Time) {
	if len(r.seenEvents) < 2000 {
		return
	}
	for id, at := range r.seenEvents {
		if now.Sub(at) > 6*time.Hour {
			delete(r.seenEvents, id)
		}
	}
}

// noteToken registers a sighting and reports whether the token is new to the recorder.
func (r *Recorder) noteToken(ctx context.Context, mint, ticker, image, source string, now time.Time, activity bool) bool {
	r.mu.Lock()
	st, known := r.tokens[mint]
	if !known {
		st = &tokenState{source: source, firstSeen: now, lastActivity: now}
		r.tokens[mint] = st
	} else if activity || source == SourceTrending {
		st.lastActivity = now
	}
	r.mu.Unlock()

	if !known || activity {
		inserted, err := r.db.UpsertToken(ctx, db.Token{
			Address: mint, NetworkID: r.cfg.NetworkID, Ticker: ticker, ImageURL: image,
			Source: source, RunID: r.runID, At: now,
		})
		r.dbErr("upsert token", err)
		if !known && !inserted {
			// Known from an earlier run: the database keeps its first sighting.
			r.log.Debug("token seen in an earlier run", "token", mint)
		}
	}
	return !known
}

// ---- scheduling ----

func (r *Recorder) active(t *tokenState, now time.Time) bool {
	return r.cfg.TrackFor <= 0 || now.Sub(t.lastActivity) <= r.cfg.TrackFor
}

// young: the full thesis list is known and started recently.
func (r *Recorder) young(t *tokenState, now time.Time) bool {
	return t.complete && !t.firstThesis.IsZero() && now.Sub(t.firstThesis) <= r.cfg.NewTokenTrackFor
}

func (r *Recorder) scheduleRefresh(mint string, prio int, now time.Time, reason string) {
	r.mu.Lock()
	due := now
	if st := r.tokens[mint]; st != nil && !st.lastRefresh.IsZero() {
		if next := st.lastRefresh.Add(r.cfg.RefreshMinInterval); next.After(due) {
			due = next
		}
	}
	r.mu.Unlock()
	r.queue.push(job{kind: jobRefresh, mint: mint, prio: prio, pages: 1, due: due, reason: reason})
}

func (r *Recorder) scheduleLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.scheduleDue(r.now())
		}
	}
}

func (r *Recorder) scheduleDue(now time.Time) {
	type plan struct {
		mint    string
		prio    int
		reason  string
		details bool
	}
	var plans []plan
	r.mu.Lock()
	for mint, st := range r.tokens {
		if !r.active(st, now) {
			continue
		}
		since := now.Sub(st.lastRefresh)
		switch {
		case st.lastRefresh.IsZero():
			// history job not done yet
		case r.young(st, now) && r.cfg.NewTokenRefresh > 0 && since >= r.cfg.NewTokenRefresh:
			plans = append(plans, plan{mint: mint, prio: prioUrgent, reason: "young token"})
		case st.count == 0:
			// No theses yet: the feed reports the first one, no need to poll.
		case r.cfg.AllTokensRefresh > 0 && since >= r.cfg.AllTokensRefresh:
			plans = append(plans, plan{mint: mint, prio: prioBackfill, reason: "periodic"})
		}
		if r.cfg.DetailsInterval > 0 && now.Sub(st.detailsAt) >= r.cfg.DetailsInterval {
			plans = append(plans, plan{mint: mint, details: true})
		}
	}
	r.mu.Unlock()
	for _, p := range plans {
		if p.details {
			r.queue.push(job{kind: jobDetails, mint: p.mint, prio: prioLow, due: now})
		} else {
			r.queue.push(job{kind: jobRefresh, mint: p.mint, prio: p.prio, pages: 1, due: now, reason: p.reason})
		}
	}
}

// ---- worker ----

func (r *Recorder) worker(ctx context.Context) {
	for {
		j, ok := r.queue.pop(ctx, r.now)
		if !ok {
			return
		}
		r.runJob(ctx, j)
	}
}

func (r *Recorder) runJob(ctx context.Context, j job) {
	var err error
	switch j.kind {
	case jobHistory, jobRefresh:
		err = r.checkTheses(ctx, j)
	case jobDetails:
		err = r.fetchDetails(ctx, j.mint)
	}
	if err == nil || ctx.Err() != nil {
		return
	}
	r.Stats.APIErrors.Add(1)
	var httpErr *fomo.HTTPError
	if errors.As(err, &httpErr) && httpErr.Status >= 400 && httpErr.Status < 500 {
		r.log.Warn("token request rejected", "kind", j.kind, "token", j.mint, "err", err)
		return
	}
	j.attempt++
	if j.attempt < 3 {
		j.due = r.now().Add(r.cfg.RetryDelay)
		r.queue.push(j)
	}
	r.log.Warn("token request failed", "kind", j.kind, "token", j.mint, "attempt", j.attempt, "err", err)
}

func (r *Recorder) checkTheses(ctx context.Context, j job) error {
	r.mu.Lock()
	st := r.tokens[j.mint]
	if st != nil && st.source == SourceTrending && j.kind == jobHistory {
		j.pages = min(j.pages, r.cfg.TrendingPages)
	}
	r.mu.Unlock()

	var (
		lastID   string
		count    int
		hasNext  bool
		complete bool
		oldest   time.Time
		items    int
	)
	for page := 0; page < max(j.pages, 1); page++ {
		p, err := r.api.TokenThesis(ctx, j.mint, lastID)
		if err != nil {
			if page == 0 {
				return err
			}
			r.log.Warn("thesis history page failed", "token", j.mint, "page", page+1, "err", err)
			break
		}
		now := r.now()
		if page == 0 {
			count, hasNext = p.Count, p.HasNextPage
		}
		var changed []db.Thesis
		r.mu.Lock()
		for _, raw := range p.Raw {
			th := tokenThesis(raw, now)
			if th.TokenAddress == "" {
				th.TokenAddress = j.mint
			}
			if !th.CreatedAt.IsZero() && (oldest.IsZero() || th.CreatedAt.Before(oldest)) {
				oldest = th.CreatedAt
			}
			if r.changed(&th) {
				changed = append(changed, th)
			}
		}
		r.mu.Unlock()
		items += len(p.Raw)
		r.dbErr("save theses", r.db.SaveTheses(ctx, changed))
		r.Stats.Observations.Add(int64(len(changed)))
		if !p.HasNextPage {
			complete = true
			break
		}
		if p.LastID == "" || p.LastID == lastID {
			break
		}
		lastID = p.LastID
	}
	now := r.now()
	r.Stats.TokenChecks.Add(1)
	r.dbErr("save count", r.db.SaveCount(ctx, db.Count{
		Token: j.mint, At: now, Count: count, HasNextPage: hasNext, Reason: j.kindName(),
		FirstThesisAt: oldest, Complete: complete,
	}))

	r.mu.Lock()
	if st != nil {
		st.lastRefresh = now
		st.count = count
		if complete {
			st.complete = true
		}
		if !oldest.IsZero() && (st.firstThesis.IsZero() || oldest.Before(st.firstThesis)) {
			st.firstThesis = oldest
		}
		if !st.announced && st.complete && !st.firstThesis.IsZero() &&
			!st.firstThesis.Before(r.startedAt.Add(-startGrace)) {
			st.announced = true
			r.log.Info("token born while recording", "token", j.mint, "theses", count,
				"first_thesis", st.firstThesis.Format(time.RFC3339))
		}
	}
	r.mu.Unlock()
	r.log.Debug("thesis check", "token", j.mint, "kind", j.kindName(), "reason", j.reason,
		"count", count, "items", items, "complete", complete)
	return nil
}

func (r *Recorder) fetchDetails(ctx context.Context, mint string) error {
	data, err := r.api.TokenDetails(ctx, mint)
	now := r.now()
	r.mu.Lock()
	if st := r.tokens[mint]; st != nil {
		st.detailsAt = now // also on failure: retry at the next interval, not in a loop
	}
	r.mu.Unlock()
	if err != nil {
		return err
	}
	r.Stats.Details.Add(1)
	r.dbErr("save details", r.db.SaveDetails(ctx, mint, now, data))
	return nil
}

// ---- snapshots ----

func (r *Recorder) snapshotLoop(ctx context.Context) {
	t := time.NewTicker(r.cfg.SnapshotInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.snapshotOnce(ctx)
		}
	}
}

func (r *Recorder) snapshotOnce(ctx context.Context) {
	now := r.now()
	r.mu.Lock()
	var mints []string
	for mint, st := range r.tokens {
		if r.active(st, now) {
			mints = append(mints, mint)
		}
	}
	r.mu.Unlock()
	slices.Sort(mints)
	for chunk := range chunks(mints, r.cfg.SnapshotBatch) {
		data, err := r.api.FilterTokens(ctx, chunk)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			r.Stats.APIErrors.Add(1)
			r.log.Warn("filterTokens failed", "tokens", len(chunk), "err", err)
			continue
		}
		parts := splitByToken(data, chunk)
		// Keep the whole response when it cannot be split, and once per run
		// as a reference of the format.
		if !r.snapshotRawSaved.Load() || len(parts) == 0 {
			r.snapshotRawSaved.Store(true)
			r.dbErr("save api response", r.db.SaveAPIResponse(ctx, "filterTokens", r.now(), chunk, data))
		}
		if len(parts) == 0 {
			r.log.Warn("filterTokens response not split by token, stored whole", "tokens", len(chunk))
			continue
		}
		if len(parts) < len(chunk) {
			r.log.Debug("filterTokens returned fewer tokens than asked", "asked", len(chunk), "got", len(parts))
		}
		r.Stats.Snapshots.Add(int64(len(parts)))
		r.dbErr("save snapshots", r.db.SaveSnapshots(ctx, "filterTokens", r.now(), parts))
	}
}

// ---- trending ----

// OnTrending records a stored trending sample: the tokens it lists in order.
func (r *Recorder) OnTrending(ctx context.Context, at time.Time, raw json.RawMessage, mints []string) {
	r.dbErr("save trending", r.db.SaveTrending(ctx, at, raw, mints))
	r.Stats.TrendingSaved.Add(1)
	for _, m := range mints {
		if r.noteToken(ctx, m, "", "", SourceTrending, at, false) {
			r.queue.push(job{kind: jobHistory, mint: m, prio: prioBackfill, pages: r.cfg.TrendingPages, due: at})
		}
	}
}

func (r *Recorder) dbErr(what string, err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	r.Stats.DBErrors.Add(1)
	r.log.Error("database write failed", "what", what, "err", err)
}

func chunks(list []string, size int) func(func([]string) bool) {
	return func(yield func([]string) bool) {
		for i := 0; i < len(list); i += size {
			if !yield(list[i:min(i+size, len(list))]) {
				return
			}
		}
	}
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
