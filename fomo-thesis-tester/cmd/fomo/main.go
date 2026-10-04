package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"fomo-thesis-tester/internal/browser"
	"fomo-thesis-tester/internal/config"
	"fomo-thesis-tester/internal/fomo"
	logpkg "fomo-thesis-tester/internal/logger"
	"fomo-thesis-tester/internal/scheduler"
	"fomo-thesis-tester/internal/state"
)

func main() {
	var cfgPath, tokensPath, mode, durationOverride string
	flag.StringVar(&cfgPath, "config", "config.json", "path to config")
	flag.StringVar(&tokensPath, "tokens", "tokens.json", "path to token list")
	flag.StringVar(&mode, "mode", "run", "run or login")
	flag.StringVar(&durationOverride, "duration", "", "optional run duration override, e.g. 3h30m")
	flag.Parse()

	cfg, err := config.Load(cfgPath)
	must(err)

	if durationOverride != "" {
		d, err := time.ParseDuration(durationOverride)
		must(err)
		cfg.RunDuration = d
	}
	lb, err := logpkg.New(cfg.LogDir)
	must(err)
	defer lb.Close()
	log := lb.Log

	root, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	accounts, browsers, err := buildAccounts(root, cfg, log)
	must(err)
	defer func() {
		for _, b := range browsers {
			b.Close()
		}
	}()

	if mode == "login" {
		log.Info("login_mode_ready", "accounts", len(accounts), "message", "Log in to each Fomo window. Press Ctrl+C when finished. Profiles persist on disk.")
		<-root.Done()
		return
	}

	tokens, err := config.LoadTokens(tokensPath)
	must(err)

	runCtx := root
	if cfg.RunDuration > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(root, cfg.RunDuration)
		defer cancel()
	}

	testStartedAt := time.Now().UTC()
	log.Info("test_window_started", "started_at", testStartedAt.Format(time.RFC3339Nano), "duration", cfg.RunDuration.String())
	runner := &scheduler.Runner{
		Pool:           &scheduler.Pool{Accounts: accounts},
		Tokens:         tokens,
		Tracker:        state.NewTracker(testStartedAt),
		Interval:       cfg.PollInterval,
		RequestTimeout: cfg.RequestTimeout,
		Lookback:       cfg.ThesisLookback,
		AuditLookback:  cfg.AuditLookback,
		AuditEvery:     cfg.AuditEvery,
		Limit:          cfg.ThesisLimit,
		Threshold:      cfg.ThesisThreshold,
		Log:            log,
	}

	err = runner.Run(runCtx)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		log.Error("runner_stopped", "error", err)
		os.Exit(1)
	}
	log.Info("run_finished", "reason", runCtx.Err())
}

func buildAccounts(ctx context.Context, cfg config.Config, log *slog.Logger) ([]*scheduler.Account, []*browser.Session, error) {
	var accounts []*scheduler.Account
	var browsers []*browser.Session
	for i, ac := range cfg.Accounts {
		if ac.ID == "" || ac.ProfileDir == "" {
			return nil, nil, fmt.Errorf("invalid account config")
		}
		profile := ac.ProfileDir
		if !filepath.IsAbs(profile) {
			abs, err := filepath.Abs(profile)
			if err != nil {
				return nil, nil, err
			}
			profile = abs
		}
		debugURL := ac.DebugURL
		if debugURL == "" {
			debugURL = fmt.Sprintf("http://127.0.0.1:%d", 9221+i)
		}
		b := browser.New(browser.Options{
			AccountID:      ac.ID,
			ProfileDir:     profile,
			DebugURL:       debugURL,
			AppURL:         cfg.FomoAppURL,
			ReadyTimeout:   cfg.BrowserReadyTimeout,
			RequestTimeout: cfg.RequestTimeout,
			Log:            log,
		})
		if err := b.Start(ctx); err != nil {
			for _, started := range browsers {
				started.Close()
			}
			return nil, nil, fmt.Errorf("start account %s: %w", ac.ID, err)
		}
		client := &fomo.Client{Transport: b}
		accounts = append(accounts, scheduler.NewAccount(ac.ID, b, client, cfg.PerAccountMinInterval, cfg.AuthCooldown, cfg.RateLimitCooldown, cfg.MaxRateLimitCooldown, log))
		browsers = append(browsers, b)
	}
	return accounts, browsers, nil
}

func must(err error) {
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "fatal:", err)
	os.Exit(1)
}
