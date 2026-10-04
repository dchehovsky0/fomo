package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"fomobot/internal/config"
	"fomobot/internal/db"
	"fomobot/internal/fomo"
	"fomobot/internal/recorder"
)

func runRecord(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	acc, err := cfg.Account("")
	if err != nil {
		return err
	}
	tokens, err := newTokenSource(cfg, acc, log)
	if err != nil {
		return err
	}
	if _, err := tokens.Token(ctx); err != nil {
		return err
	}
	store, err := db.Open(ctx, cfg.Database.URL)
	if err != nil {
		return err
	}
	defer store.Close()

	started := time.Now()
	// Only non-secret settings go to the database.
	runID, err := store.StartRun(ctx, started, map[string]any{
		"recorder": cfg.Recorder, "network_id": cfg.Fomo.NetworkID, "feed_limit": cfg.Fomo.FeedLimit,
		"rate_limit_rps": cfg.Fomo.RateLimitRPS, "session_mode": cfg.Session.Mode,
	})
	if err != nil {
		return err
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := store.StopRun(stopCtx, runID, time.Now()); err != nil {
			log.Warn("mark run stopped", "err", err)
		}
	}()

	calls := make(chan db.APICall, 4096)
	client, err := newClient(cfg, acc, tokens, pageTransport(tokens), log, func(c fomo.CallInfo) {
		row := db.APICall{At: c.At, Method: c.Method, Path: c.Path, Query: c.Query,
			Status: c.Status, Duration: c.Duration, Bytes: c.Bytes}
		if c.Err != nil {
			row.Error = c.Err.Error()
		}
		select {
		case calls <- row:
		default: // journal is best effort
		}
	})
	if err != nil {
		return err
	}

	r := cfg.Recorder
	rec := recorder.New(recorder.Config{
		NetworkID:          cfg.Fomo.NetworkID,
		FeedLimit:          cfg.Fomo.FeedLimit,
		FeedInterval:       r.FeedInterval,
		HistoryMaxPages:    r.HistoryMaxPages,
		RefreshMinInterval: r.RefreshMinInterval,
		NewTokenRefresh:    r.NewTokenRefresh,
		NewTokenTrackFor:   r.NewTokenTrackFor,
		AllTokensRefresh:   r.AllTokensRefresh,
		TrackFor:           r.TrackFor,
		SnapshotInterval:   r.SnapshotInterval,
		SnapshotBatch:      r.SnapshotBatch,
		DetailsInterval:    r.DetailsInterval,
		TrendingPages:      r.Trending.HistoryPages,
		RetryDelay:         r.RetryDelay,
	}, client, store, runID, log.With("component", "recorder"))
	if err := rec.Restore(ctx); err != nil {
		return err
	}

	var trending *recorder.Trending
	if r.Trending.Enabled {
		trending = recorder.NewTrending(recorder.TrendingConfig{
			URL: r.Trending.WSURL, TopicID: r.Trending.TopicID, Origin: fomo.Origin(cfg.Fomo.AppURL),
			UserAgent: cfg.Fomo.UserAgent, NetworkID: cfg.Fomo.NetworkID, Sample: r.Trending.Sample,
		}, tokens, rec, store, log.With("component", "trending"))
	}

	log.Info("fomo recorder started", "run_id", runID, "session", cfg.Session.Mode,
		"trending", r.Trending.Enabled, "feed_interval", r.FeedInterval)

	var wg sync.WaitGroup
	wg.Go(func() { rec.Run(ctx) })
	if trending != nil {
		wg.Go(func() { trending.Run(ctx) })
	}
	wg.Go(func() { journalCalls(ctx, store, runID, calls, log) })
	wg.Go(func() { reportRecordStats(ctx, client, store, rec, trending, log) })

	waitStop(ctx, &wg, log)
	return nil
}

func journalCalls(ctx context.Context, store *db.DB, runID int64, calls <-chan db.APICall, log *slog.Logger) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	var batch []db.APICall
	flush := func(ctx context.Context) {
		if len(batch) == 0 {
			return
		}
		if err := store.SaveAPICalls(ctx, runID, batch); err != nil {
			log.Warn("save api calls", "err", err)
		}
		batch = batch[:0]
	}
	for {
		select {
		case c := <-calls:
			batch = append(batch, c)
		case <-t.C:
			flush(ctx)
		case <-ctx.Done():
		drain:
			for {
				select {
				case c := <-calls:
					batch = append(batch, c)
				default:
					break drain
				}
			}
			fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			flush(fctx)
			cancel()
			return
		}
	}
}

func reportRecordStats(ctx context.Context, c *fomo.Client, store *db.DB, rec *recorder.Recorder, tr *recorder.Trending, log *slog.Logger) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		all, young := rec.Tracked()
		s := &rec.Stats
		args := []any{
			"requests", c.Stats.Requests.Load(), "http_429", c.Stats.RateLimited.Load(),
			"auth_errors", c.Stats.AuthErrors.Load(), "api_errors", s.APIErrors.Load(),
			"db_errors", s.DBErrors.Load(), "queue", rec.QueueLen(),
			"tracked_tokens", all, "young_tokens", young, "live_theses", s.NewTheses.Load(),
		}
		if tr != nil {
			args = append(args, "ws_messages", tr.MessageCount())
		}
		if counts, err := store.Counts(ctx); err == nil {
			for _, k := range []string{"tokens", "theses", "thesis_observations", "thesis_counts", "token_snapshots", "trending_snapshots"} {
				args = append(args, "db_"+k, counts[k])
			}
		}
		log.Info("recorder stats", args...)
	}
}
