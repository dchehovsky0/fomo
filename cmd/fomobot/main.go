package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"

	"fomobot/internal/accounts"
	"fomobot/internal/alertinfo"
	"fomobot/internal/axiom"
	"fomobot/internal/config"
	"fomobot/internal/db"
	"fomobot/internal/dexscreener"
	"fomobot/internal/fomo"
	"fomobot/internal/launches"
	"fomobot/internal/notify"
	"fomobot/internal/screen"
	"fomobot/internal/session"
	"fomobot/internal/store"
	"fomobot/internal/trending"
	"fomobot/internal/watcher"
	"fomobot/internal/web"
)

func main() {
	var (
		configPath   = flag.String("config", "config.yaml", "path to config file")
		envPath      = flag.String("env", "", "path to .env file with secrets (default: .env next to the config)")
		login        = flag.Bool("login", false, "open Chrome on an account's profile to log in to fomo.family (session.mode=chromedp)")
		loginAll     = flag.Bool("login-all", false, "check every account and open Chrome, one by one, for those without a fomo login")
		account      = flag.String("account", "", "account name for -login (default: the first account)")
		axiomLogin   = flag.Bool("axiom-login", false, "open Chrome on the Axiom profile to log in to axiom.trade")
		check        = flag.Bool("check", false, "wait for a new token from Axiom, check every fomo account once and exit")
		testTelegram = flag.Bool("test-telegram", false, "send a sample signal to Telegram and exit")
		dryRun       = flag.Bool("dry-run", false, "print signals to the console instead of Telegram")
		record       = flag.Bool("record", false, "record everything seen on fomo to PostgreSQL (database.url), no alerts")
	)
	flag.Parse()

	if *envPath == "" {
		*envPath = filepath.Join(filepath.Dir(*configPath), ".env")
	}
	if err := config.LoadDotEnv(*envPath); err != nil {
		fmt.Fprintln(os.Stderr, "load .env:", err)
		os.Exit(1)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	log, logPath, closeLog := newLogger(cfg.Log)
	defer closeLog()
	log.Info("log file opened", "path", logPath)
	session.SetChromeConcurrency(cfg.Session.ChromeConcurrency)

	ctx, stop := listenSignals()
	defer stop()

	switch {
	case *login:
		err = runLogin(ctx, cfg, *account, log)
	case *loginAll:
		err = runLoginAll(ctx, cfg, log)
	case *axiomLogin:
		err = runAxiomLogin(ctx, cfg)
	case *testTelegram:
		err = runTestTelegram(ctx, cfg, log)
	case *check:
		err = runCheck(ctx, cfg, log)
	case *record:
		err = runRecord(ctx, cfg, log)
	default:
		err = runBot(ctx, cfg, log, *dryRun)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func runBot(ctx context.Context, cfg config.Config, log *slog.Logger, dryRun bool) error {
	ax, err := newAxiom(cfg, log)
	if err != nil {
		return err
	}
	loc, err := cfg.Location()
	if err != nil {
		return err
	}
	clients := make([]*fomo.Client, 0, len(cfg.Accounts))
	watched := make([]watcher.Account, 0, len(cfg.Accounts))
	checkers := make([]alertinfo.Checker, 0, len(cfg.Accounts))
	allTokens := make([]session.TokenSource, 0, len(cfg.Accounts))
	health := accounts.NewRegistry(log.With("component", "accounts"))
	var capacity float64
	logProxySpread(log, cfg.Accounts)
	for _, acc := range cfg.Accounts {
		h := health.Add(acc.Name, acc.Proxy, acc.RPS)
		client, tokens, err := newTrackedClient(cfg, acc, h, log)
		if err != nil {
			return fmt.Errorf("account %s: %w", acc.Name, err)
		}
		allTokens = append(allTokens, tokens)
		clients = append(clients, client)
		checkers = append(checkers, client)
		watched = append(watched, watcher.Account{Name: acc.Name, Client: client, Workers: acc.Workers})
		capacity += acc.RPS
	}

	statePath := cfg.Store.Path
	if dryRun {
		// A dry run must not mark tokens as alerted for the real bot.
		statePath = ""
	}
	st, err := store.Open(statePath)
	if err != nil {
		return err
	}
	var pages *web.Server
	var pagesSink notify.Pages
	if cfg.Web.Enabled {
		if pages, err = newPages(cfg, loc, log); err != nil {
			return err
		}
		pagesSink = pages
		pages.HandleAdmin("GET /health/accounts", health.Handler())
		pages.HandleAdmin("GET /health/resources", web.Resources())
	}

	var notifier watcher.Notifier
	if dryRun || cfg.Telegram.BotToken == "" || cfg.Telegram.ChatID == "" {
		if !dryRun {
			log.Warn("telegram bot_token/chat_id not set, printing signals to console")
		}
		notifier = notify.NewConsole(os.Stdout, loc, pagesSink, log)
	} else {
		notifier = notify.NewTelegram(cfg.Telegram.BotToken, cfg.Telegram.ChatID, loc, pagesSink, log)
	}

	src := launches.New(ax, launches.Config{
		Protocols: cfg.Axiom.Protocols, SkipProtocols: cfg.Axiom.SkipProtocols, Lifetime: cfg.Watch.Lifetime,
	}, log.With("component", "launches"))
	info := newAlertInfo(cfg, checkers, src.Lookup, log)

	wc := watcher.Config{
		Lifetime:    cfg.Watch.Lifetime,
		VolumeFor:   cfg.Watch.VolumeFor,
		MinTheses:   cfg.Watch.MinTheses,
		ThesisLimit: cfg.Watch.ThesisLimit,
		RetryDelay:  cfg.Watch.RetryDelay,
		AuthPause:   cfg.Watch.AuthPause,
		Complete:    info.Complete,
	}
	for _, t := range cfg.Watch.Schedule {
		wc.Schedule = append(wc.Schedule, watcher.Tier{MaxAge: t.MaxAge, Every: t.Every})
	}
	w := watcher.New(wc, watched, notifier, st, log.With("component", "watcher"))
	if !dryRun {
		tiers, closeTiers, err := openWatchDB(ctx, cfg, log)
		if err != nil {
			return err
		}
		defer closeTiers()
		w.SetTiers(tiers)
	}
	health.SetDemand(w.DemandRPS)
	health.SetWatching(w.Watching)
	if pages != nil {
		pages.HandleAdmin("GET /health/alerts", w.DelayHandler())
		pages.HandleAdmin("GET /api/board", w.BoardHandler(cfg.Telegram.TokenLinkTemplate, cfg.Telegram.AxiomLinkTemplate))
		pages.HandleAdmin("GET /api/watch/{mint}", w.LiveHandler(cfg.Telegram.TokenLinkTemplate, cfg.Telegram.AxiomLinkTemplate))
	}
	gate, err := newScreen(cfg, log)
	if err != nil {
		return err
	}
	if gate != nil {
		health.SetDexScreener(func() any { return gate.Health() })
	}

	var stream *trending.Stream
	var alerter *trending.Alerter
	if cfg.Trending.Enabled {
		stream = trending.NewStream(trending.StreamConfig{
			URL: cfg.Trending.WSURL, TopicID: cfg.Trending.TopicID,
			Origin: fomo.Origin(cfg.Fomo.AppURL), UserAgent: cfg.Fomo.UserAgent,
		}, session.NewFailover(allTokens...), log.With("component", "trending"))
		alerter = trending.NewAlerter(trending.Config{
			NetworkID: cfg.Fomo.NetworkID, RealertAfter: cfg.Trending.RealertAfter,
			SilentFirstSnapshot: cfg.Trending.SilentFirstSnapshot,
			RetryDelay:          cfg.Watch.RetryDelay, Complete: info.Complete,
			Skip: func(mint, launchpad string) bool {
				return src.Skips(launchpad) || src.Blocked(mint)
			},
		}, st, notifier, log.With("component", "trending"))
	}

	protocols := "all"
	if len(cfg.Axiom.Protocols) > 0 {
		protocols = strings.Join(cfg.Axiom.Protocols, ",")
	}
	log.Info("fomo thesis bot started",
		"lifetime", cfg.Watch.Lifetime, "min_theses", cfg.Watch.MinTheses, "protocols", protocols,
		"skip_protocols", strings.Join(cfg.Axiom.SkipProtocols, ","),
		"accounts", len(watched), "capacity_rps", capacity, "session", cfg.Session.Mode,
		"trending_alerts", cfg.Trending.Enabled, "signaled_tokens", st.Signals(), "dry_run", dryRun)

	var wg sync.WaitGroup
	if gate != nil {
		wg.Go(func() { src.Run(ctx, gate.Add) })
		wg.Go(func() { gate.Run(ctx, w.Add) })
		wg.Go(func() { gate.Recheck(ctx, w) })
	} else {
		wg.Go(func() { src.Run(ctx, w.Add) })
	}
	wg.Go(func() { w.Run(ctx) })
	wg.Go(func() { health.Run(ctx, healthEvery, healthIdleAfter) })
	if pages != nil {
		log.Info("fomo accounts status", "url", healthURL(cfg))
		log.Info("process resources", "url", strings.TrimSuffix(healthURL(cfg), "/accounts")+"/resources")
		log.Info("watch board", "url", strings.TrimSuffix(healthURL(cfg), "/health/accounts")+"/")
		wg.Go(func() {
			if err := pages.Run(ctx, webListen(cfg)); err != nil && ctx.Err() == nil {
				log.Error("theses pages server stopped", "err", err)
			}
		})
	}
	if stream != nil {
		wg.Go(func() { stream.Run(ctx, alerter.Handle) })
		wg.Go(func() { alerter.Run(ctx) })
	}
	wg.Go(func() {
		st.Run(ctx, cfg.Store.FlushInterval, 2*cfg.Watch.Lifetime+time.Hour, cfg.Trending.RealertAfter, log)
	})
	wg.Go(func() { reportStats(ctx, src, ax, w, stream, alerter, clients, st, capacity, log) })

	waitStop(ctx, &wg, log)
	return nil
}

func reportStats(ctx context.Context, src *launches.Source, ax *axiom.Stream, w *watcher.Watcher, stream *trending.Stream, alerter *trending.Alerter,
	clients []*fomo.Client, st *store.Store, capacity float64, log *slog.Logger) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		var requests, limited, authErrs, httpNanos, httpSamples int64
		for _, c := range clients {
			requests += c.Stats.Requests.Load()
			limited += c.Stats.RateLimited.Load()
			authErrs += c.Stats.AuthErrors.Load()
			httpNanos += c.Stats.HTTPNanos.Load()
			httpSamples += c.Stats.HTTPSamples.Load()
		}
		snap := w.Snapshot()
		ss, ws, as := &src.Stats, &w.Stats, &ax.Stats
		var pairAgo time.Duration
		if last := as.LastPair.Load(); last > 0 {
			pairAgo = time.Since(time.UnixMilli(last)).Round(time.Second)
		}
		log.Info("stats",
			"launches", ss.Emitted.Load(), "pairs", ss.Received.Load(), "last_pair_ago", pairAgo,
			"watching", snap.Watching, "demand_rps", fmt.Sprintf("%.1f", snap.DemandRPS), "capacity_rps", capacity,
			"overdue", snap.Overdue, "avg_lag", snap.AvgLag.Round(time.Second), "max_lag", snap.MaxLag.Round(time.Second),
			"checks", ws.Checks.Load(), "check_errors", ws.Errors.Load(), "signals", ws.Signals.Load(),
			"send_errors", ws.SendErrors.Load(), "expired", ws.Expired.Load(),
			"avg_check", avgDuration(ws.CheckNanos.Load(), ws.CheckSamples.Load()),
			"requests", requests, "avg_http", avgDuration(httpNanos, httpSamples),
			"http_429", limited, "auth_errors", authErrs, "signaled_tokens", st.Signals())
		log.Info("axiom stats",
			"protocols", src.Protocols(), "duplicates", ss.Duplicates.Load(), "filtered", ss.Filtered.Load(),
			"connects", as.Connects.Load(), "chrome_restarts", as.Restarts.Load(), "bad_messages", as.Bad.Load(),
			"dropped", as.Dropped.Load())
		if stream != nil {
			var ago time.Duration
			if last := stream.Stats.LastMessage.Load(); last > 0 {
				ago = time.Since(time.UnixMilli(last)).Round(time.Second)
			}
			log.Info("trending stats",
				"messages", stream.Stats.Messages.Load(), "last_message_ago", ago, "reconnects", stream.Stats.Reconnects.Load(),
				"alerts", alerter.Stats.Alerts.Load(), "baseline", alerter.Stats.Baseline.Load(),
				"send_errors", alerter.Stats.SendErrors.Load(), "lost", alerter.Stats.Lost.Load(),
				"known_tokens", st.TrendingTokens())
		}
		if snap.DemandRPS > capacity*1.05 {
			log.Warn("not enough accounts: tokens are checked less often than scheduled",
				"demand_rps", fmt.Sprintf("%.1f", snap.DemandRPS), "capacity_rps", capacity,
				"accounts_needed", int(snap.DemandRPS/(capacity/float64(len(clients)))+0.999))
		}
	}
}

// runCheck waits for one new token from Axiom and makes one thesis request
// per account with it.
func runCheck(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	ax, err := newAxiom(cfg, log)
	if err != nil {
		return err
	}
	fmt.Println("Жду новый токен из Axiom. Бот не должен работать: Chrome не открывает один профиль дважды.")
	wait := cfg.Axiom.StartTimeout + cfg.Axiom.StallTimeout
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	started := time.Now()
	var sample axiom.Pair
	ax.Run(waitCtx, func(p axiom.Pair) {
		if sample.Token == "" {
			sample = p
			cancel()
		}
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if sample.Token == "" {
		return fmt.Errorf("no new pair from Axiom in %s: log in with fomobot -axiom-login or set AXIOM_* cookies in .env", wait)
	}
	created := sample.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	fmt.Printf("Axiom: первый токен через %s — %s (%s, %s), создан %s назад\n\n",
		time.Since(started).Round(time.Second), sample.Token, sample.Ticker, sample.Label(), time.Since(created).Round(time.Second))

	var failed int
	for _, acc := range cfg.Accounts {
		client, _, err := newAccountClient(cfg, acc, log, nil)
		if err == nil {
			var page *fomo.TokenThesisPage
			page, err = client.SortedThesis(ctx, sample.Token, created.Add(-time.Minute), time.Now(), cfg.Watch.ThesisLimit)
			if err == nil {
				fmt.Printf("  аккаунт %-12s OK: count=%d, получено тезисов %d\n", acc.Name, page.Count, len(page.Items))
				continue
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		failed++
		fmt.Printf("  аккаунт %-12s ОШИБКА: %v\n", acc.Name, err)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d accounts failed", failed, len(cfg.Accounts))
	}
	return nil
}

func runTestTelegram(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	if cfg.Telegram.BotToken == "" || cfg.Telegram.ChatID == "" {
		return errors.New("set TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID")
	}
	loc, err := cfg.Location()
	if err != nil {
		return err
	}
	var pages notify.Pages
	if cfg.Web.Enabled {
		if pages, err = newPages(cfg, loc, log); err != nil {
			return err
		}
	}
	tg := notify.NewTelegram(cfg.Telegram.BotToken, cfg.Telegram.ChatID, loc, pages, log)
	if err := tg.SendText(ctx, "✅ fomo thesis bot: тестовое сообщение, так будет выглядеть алерт 👇"); err != nil {
		return err
	}
	if err := tg.Send(ctx, sampleAlert(cfg)); err != nil {
		return err
	}
	fmt.Println("Тестовый алерт отправлен в Telegram.")
	if pages != nil {
		fmt.Println("Страница с тезисами откроется, пока запущен бот (fomobot или fomobot -dry-run).")
	}
	return nil
}

func sampleAlert(cfg config.Config) notify.Alert {
	now := time.Now()
	created := now.Add(-42 * time.Minute)
	token := "So11111111111111111111111111111111111111112"
	a := notify.Alert{
		Kind: notify.KindTheses, Threshold: cfg.Watch.MinTheses, Token: token, Symbol: "TEST", Name: "Test Token",
		Chain: "Solana", Dex: "pumpfun", CreatedAt: created, DetectedAt: now, MarketCap: 1_200_000,
		CountKnown: true, Count: 3, RateKnown: true, Recent: 3, RateWindow: cfg.Watch.RateWindow,
		FirstExact: true,
		FomoURL:    strings.NewReplacer("{mint}", token, "{network}", cfg.Fomo.NetworkID).Replace(cfg.Telegram.TokenLinkTemplate),
		AxiomURL:   strings.NewReplacer("{pair}", token, "{mint}", token).Replace(cfg.Telegram.AxiomLinkTemplate),
		First: []notify.Thesis{
			{At: created.Add(12 * time.Minute), Handle: "prometheusx91", Verified: true, PositionUSD: 27982.8, MarketCap: 640_000,
				Text: "Dev is based, community is cooking.\nChart looks like early $WIF — loading a bag here."},
			{At: created.Add(31 * time.Minute), Handle: "example_trader", PositionUSD: 640, MarketCap: 910_000,
				Text: "Strong holders, no bundles. Target 5M."},
			{At: created.Add(40 * time.Minute), Handle: "quick_flip", Closed: true, MarketCap: 1_150_000,
				Text: "Took profit, still bullish long term."},
		},
	}
	return a
}

func runLogin(ctx context.Context, cfg config.Config, name string, log *slog.Logger) error {
	acc, err := cfg.Account(name)
	if err != nil {
		return err
	}
	fmt.Printf("Аккаунт %s, профиль Chrome: %s\n", acc.Name, acc.ChromeProfileDir)
	return loginAccount(ctx, cfg, acc, log)
}

func loginAccount(ctx context.Context, cfg config.Config, acc config.Account, log *slog.Logger) error {
	opts := chromeOptions(cfg, acc)
	if acc.DebugURL != "" {
		return loginAttached(ctx, cfg, acc, opts, log)
	}
	fmt.Println("1. Войди в аккаунт fomo.family в открывшемся окне.")
	fmt.Println("2. Дождись, пока загрузится лента, и полностью закрой это окно Chrome.")
	if err := session.Login(ctx, opts); err != nil {
		return err
	}
	fmt.Println("Окно закрыто, проверяю токен в headless-режиме...")
	src, err := session.NewChrome(opts, log)
	if err != nil {
		return err
	}
	tok, exp, err := src.Fetch(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Готово: токен получен (%s…), действует до %s.\n", tok[:min(12, len(tok))], exp.Local().Format(time.DateTime))
	return nil
}

// loginAttached signs in without a debug port: Google refuses the login when
// remote debugging is on. The bot later attaches to the same profile only
// after that Chrome is opened again with debug_url.
func loginAttached(ctx context.Context, cfg config.Config, acc config.Account, opts session.ChromeOptions, log *slog.Logger) error {
	if debugReachable(acc.DebugURL) {
		src, err := session.NewChrome(opts, log)
		if err != nil {
			return err
		}
		tok, exp, err := src.Fetch(ctx)
		if err != nil {
			fmt.Println("Окно на этом порту уже открыто, но токена Fomo в нём нет. Войди на fomo.family в этом окне и оставь его открытым.")
			return err
		}
		fmt.Printf("Готово: токен получен (%s…), действует до %s.\n", tok[:min(12, len(tok))], exp.Local().Format(time.DateTime))
		return nil
	}
	fmt.Println("1. Войди в аккаунт fomo.family в открывшемся окне.")
	fmt.Println("2. Дождись, пока загрузится лента, и полностью закрой это окно Chrome.")
	fmt.Println("Порт отладки сейчас выключен: с ним Google не пускает на вход.")
	if err := session.Login(ctx, opts); err != nil {
		return err
	}
	fmt.Println("Вход сохранён в профиле. Открой это окно ещё раз и не закрывай его, пока работает бот:")
	fmt.Println(chromeDebugCommand(cfg, acc))
	return nil
}

func chromeDebugCommand(cfg config.Config, acc config.Account) string {
	path := cfg.Session.ChromePath
	if path == "" {
		path = session.FindChrome()
	}
	if path == "" {
		path = "chrome"
	}
	app := strings.TrimRight(cfg.Fomo.AppURL, "/")
	if !strings.Contains(strings.ToLower(app), "/token") {
		app += "/token"
	}
	port := acc.DebugURL
	if u, err := url.Parse(acc.DebugURL); err == nil && u.Port() != "" {
		port = u.Port()
	}
	return fmt.Sprintf("%s --user-data-dir=%s --remote-debugging-port=%s --no-first-run --no-default-browser-check %s",
		quoteArg(path), quoteArg(acc.ChromeProfileDir), port, app)
}

func quoteArg(s string) string {
	if strings.ContainsAny(s, " \t") {
		return `"` + s + `"`
	}
	return s
}

func debugReachable(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	conn, err := net.DialTimeout("tcp", u.Host, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// loginCheckTimeout: a logged-in profile yields a token within seconds.
const loginCheckTimeout = 25 * time.Second

// runLoginAll walks every account: a profile that already yields a token is
// skipped, the others open Chrome for a manual login, one at a time.
func runLoginAll(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	if cfg.Session.Mode != "chromedp" {
		return errors.New("-login-all needs session.mode: chromedp")
	}
	fmt.Println("Бот не должен работать в это время: Chrome не открывает один профиль дважды.")
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	var had, added, failed []string
	for i, acc := range cfg.Accounts {
		fmt.Printf("\n[%d/%d] %s — %s\n", i+1, len(cfg.Accounts), acc.Name, acc.ChromeProfileDir)
		if acc.DebugURL != "" && !debugReachable(acc.DebugURL) {
			fmt.Println("  Chrome на порту отладки не запущен, открываю окно для входа.")
			if err := loginAccount(ctx, cfg, acc, log); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				fmt.Printf("  не получилось: %v\n", err)
				failed = append(failed, acc.Name)
				continue
			}
			added = append(added, acc.Name)
			continue
		}
		if _, err := os.Stat(acc.ChromeProfileDir); err == nil {
			opts := chromeOptions(cfg, acc)
			opts.LoadTimeout = loginCheckTimeout
			if src, err := session.NewChrome(opts, quiet); err == nil {
				if _, exp, err := src.Fetch(ctx); err == nil {
					fmt.Printf("  вход уже есть, токен до %s\n", exp.Local().Format(time.TimeOnly))
					had = append(had, acc.Name)
					continue
				}
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		fmt.Println("  входа нет, открываю Chrome.")
		if err := loginAccount(ctx, cfg, acc, log); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fmt.Printf("  не получилось: %v\n", err)
			failed = append(failed, acc.Name)
			continue
		}
		added = append(added, acc.Name)
	}
	fmt.Printf("\nИтого: вход уже был — %d, вошли сейчас — %d, не получилось — %d из %d.\n",
		len(had), len(added), len(failed), len(cfg.Accounts))
	if len(failed) > 0 {
		fmt.Println("Повтори для них: fomobot -login -account " + strings.Join(failed, " / "))
		return fmt.Errorf("%d accounts without a fomo login", len(failed))
	}
	return nil
}

// runAxiomLogin opens a plain Chrome on the Axiom profile for a manual login;
// the bot then reuses that session and the site keeps refreshing it.
func runAxiomLogin(ctx context.Context, cfg config.Config) error {
	fmt.Printf("Профиль Chrome для Axiom: %s\n", cfg.Axiom.ProfileDir)
	fmt.Println("1. Войди в axiom.trade в открывшемся окне.")
	fmt.Println("2. Дождись, пока загрузится Pulse, и полностью закрой это окно Chrome.")
	err := session.Login(ctx, session.ChromeOptions{
		AppURL: cfg.Axiom.AppURL, ProfileDir: cfg.Axiom.ProfileDir, ChromePath: cfg.Session.ChromePath,
	})
	if err != nil {
		return err
	}
	fmt.Println("Готово. Проверить поток новых токенов: fomobot -check")
	return nil
}

func newAxiom(cfg config.Config, log *slog.Logger) (*axiom.Stream, error) {
	a := cfg.Axiom
	return axiom.NewStream(axiom.Config{
		AppURL:   a.AppURL,
		Clusters: a.Clusters,
		Chrome: session.ChromeOptions{
			ProfileDir: a.ProfileDir, ChromePath: cfg.Session.ChromePath,
			Headless: a.Headless, NoSandbox: cfg.Session.NoSandbox,
		},
		Cookies: map[string]string{
			"auth-access-token":  a.AccessToken,
			"auth-refresh-token": a.RefreshToken,
			"cf_clearance":       a.CFClearance,
		},
		StallTimeout: a.StallTimeout,
		StartTimeout: a.StartTimeout,
	}, log.With("component", "axiom"))
}

func newTokenSource(cfg config.Config, acc config.Account, log *slog.Logger) (session.TokenSource, error) {
	log = log.With("component", "session", "account", acc.Name)
	switch cfg.Session.Mode {
	case "chromedp":
		return session.NewChrome(chromeOptions(cfg, acc), log)
	default:
		return session.NewStatic(acc.AccessToken, acc.TokenFile, log), nil
	}
}

func chromeOptions(cfg config.Config, acc config.Account) session.ChromeOptions {
	return session.ChromeOptions{
		AccountID:      acc.Name,
		AppURL:         cfg.Fomo.AppURL,
		ProfileDir:     acc.ChromeProfileDir,
		ChromePath:     cfg.Session.ChromePath,
		DebugURL:       acc.DebugURL,
		Headless:       cfg.Session.Headless,
		NoSandbox:      cfg.Session.NoSandbox,
		RefreshBefore:  cfg.Session.RefreshBeforeExpiry,
		LoadTimeout:    cfg.Session.PageLoadTimeout,
		RequestTimeout: cfg.Fomo.RequestTimeout,
	}
}

func pageTransport(src session.TokenSource) fomo.PageTransport {
	ch, ok := src.(*session.Chrome)
	if !ok || !ch.InPage() {
		return nil
	}
	return ch
}

func newAccountClient(cfg config.Config, acc config.Account, log *slog.Logger, onCall func(fomo.CallInfo)) (*fomo.Client, session.TokenSource, error) {
	tokens, err := newTokenSource(cfg, acc, log)
	if err != nil {
		return nil, nil, err
	}
	client, err := newClient(cfg, acc, tokens, pageTransport(tokens), log, onCall)
	return client, tokens, err
}

// Health: accounts with no successful request for healthIdleAfter get a
// probe request; the check runs every healthEvery.
const (
	healthEvery     = time.Minute
	healthIdleAfter = 5 * time.Minute
	// probeMint: any mint works, the probe only needs fomo to accept the token.
	probeMint = "So11111111111111111111111111111111111111112"
)

// newTrackedClient is newAccountClient whose requests and tokens feed the
// account's health.
func newTrackedClient(cfg config.Config, acc config.Account, h *accounts.Account, log *slog.Logger) (*fomo.Client, session.TokenSource, error) {
	raw, err := newTokenSource(cfg, acc, log)
	if err != nil {
		return nil, nil, err
	}
	tokens := h.Track(raw)
	client, err := newClient(cfg, acc, tokens, pageTransport(raw), log, h.ObserveCall)
	if err != nil {
		return nil, nil, err
	}
	h.SetProbe(func(ctx context.Context) error {
		now := time.Now()
		_, err := client.SortedThesis(ctx, probeMint, now.Add(-time.Minute), now, 1)
		return err
	})
	return client, tokens, nil
}

func healthURL(cfg config.Config) string {
	base := cfg.Web.PublicURL
	if base == "" {
		base = localURL(cfg.Web.Listen)
	}
	return strings.TrimRight(base, "/") + "/health/accounts"
}

// newPages serves theses pages; without web.public_url they are reachable
// only from this machine, at http://localhost:<port>.
func newPages(cfg config.Config, loc *time.Location, log *slog.Logger) (*web.Server, error) {
	publicURL := cfg.Web.PublicURL
	if publicURL == "" {
		publicURL = localURL(cfg.Web.Listen)
		log.Info("web.public_url is not set: theses pages open only on this computer", "url", publicURL)
	} else if cfg.Web.AdminToken == "" {
		log.Warn("web.admin_token (FOMO_ADMIN_TOKEN) is not set: /health/accounts is closed on the server")
	}
	return web.New(web.Config{
		Dir: cfg.Web.PagesDir, PublicURL: publicURL, Keep: cfg.Web.Keep,
		Exposed: cfg.Web.PublicURL != "", AdminToken: cfg.Web.AdminToken,
	}, loc, log.With("component", "web"))
}

// localURL turns a listen address like ":8080" or "0.0.0.0:8080" into http://localhost:8080.
func localURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://localhost:8080"
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "127.0.0.1" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// webListen keeps a local-only pages server off the network: without
// web.public_url nobody else can open the pages anyway.
func webListen(cfg config.Config) string {
	host, port, err := net.SplitHostPort(cfg.Web.Listen)
	if cfg.Web.PublicURL != "" || err != nil || (host != "" && host != "0.0.0.0" && host != "::") {
		return cfg.Web.Listen
	}
	return net.JoinHostPort("127.0.0.1", port)
}

func logProxySpread(log *slog.Logger, accs []config.Account) {
	counts := map[string]int{}
	var direct int
	for _, a := range accs {
		if a.Proxy == "" {
			direct++
			continue
		}
		counts[a.Proxy]++
	}
	maxN := 0
	for _, n := range counts {
		maxN = max(maxN, n)
	}
	log.Info("fomo requests use proxies", "accounts", len(accs), "proxies", len(counts), "max_per_proxy", maxN, "direct", direct)
}

// openWatchDB connects the tier marks. An empty database url leaves them off.
func openWatchDB(ctx context.Context, cfg config.Config, log *slog.Logger) (watcher.TierBook, func(), error) {
	if cfg.Database.URL == "" {
		log.Info("watch tiers stay in memory", "why", "database.url is empty")
		return nil, func() {}, nil
	}
	d, err := db.Open(ctx, cfg.Database.URL)
	if err != nil {
		return nil, nil, err
	}
	log.Info("watch tiers go to postgres")
	return d, func() { d.Close() }, nil
}

func newScreen(cfg config.Config, log *slog.Logger) (*screen.Gate, error) {
	if cfg.Screen.MinMarketCap <= 0 {
		return nil, nil
	}
	proxies, err := dexscreener.LoadProxies(cfg.Screen.Proxies)
	if err != nil {
		return nil, err
	}
	pool, err := dexscreener.NewPool(10*time.Second, proxies)
	if err != nil {
		return nil, err
	}
	log.Info("market cap screen", "proxies", len(proxies), "wave", pool.Limit(),
		"min_market_cap", cfg.Screen.MinMarketCap, "volume_min_growth", cfg.Screen.MinGrowth,
		"recheck", cfg.Screen.Recheck, "after", cfg.Screen.After)
	return screen.New(screen.Config{
		After: cfg.Screen.After, MinMarketCap: cfg.Screen.MinMarketCap,
		Recheck: cfg.Screen.Recheck, MinGrowth: cfg.Screen.MinGrowth,
	}, pool, log.With("component", "screen")), nil
}

func newAlertInfo(cfg config.Config, checkers []alertinfo.Checker, lookup alertinfo.LaunchLookup, log *slog.Logger) *alertinfo.Builder {
	return alertinfo.New(alertinfo.Config{
		FirstN:            cfg.Watch.MinTheses,
		SearchLimit:       cfg.Watch.ThesisLimit,
		RateWindow:        cfg.Watch.RateWindow,
		NetworkID:         cfg.Fomo.NetworkID,
		FomoLinkTemplate:  cfg.Telegram.TokenLinkTemplate,
		AxiomLinkTemplate: cfg.Telegram.AxiomLinkTemplate,
	}, checkers, dexscreener.New(10*time.Second), lookup, log.With("component", "alertinfo"))
}

func newClient(cfg config.Config, acc config.Account, tokens session.TokenSource, page fomo.PageTransport, log *slog.Logger, onCall func(fomo.CallInfo)) (*fomo.Client, error) {
	log = log.With("component", "fomo", "account", acc.Name)
	if page != nil {
		log.Info("fomo API calls run inside the attached Chrome tab", "debug_url", acc.DebugURL)
	}
	return fomo.NewClient(fomo.Options{
		OnCall:          onCall,
		BaseURL:         strings.TrimRight(cfg.Fomo.BaseURL, "/"),
		AppURL:          cfg.Fomo.AppURL,
		SupportedChains: cfg.Fomo.SupportedChains,
		NetworkID:       cfg.Fomo.NetworkID,
		UserAgent:       cfg.Fomo.UserAgent,
		Timeout:         cfg.Fomo.RequestTimeout,
		RPS:             acc.RPS,
		MaxRetries:      cfg.Fomo.MaxRetries,
		ThesisThreshold: cfg.Fomo.ThesisThreshold,
		Proxy:           acc.Proxy,
		Page:            page,
	}, tokens, log)
}

func avgDuration(nanos, samples int64) time.Duration {
	if samples <= 0 {
		return 0
	}
	return time.Duration(nanos / samples).Round(time.Millisecond)
}

func newLogger(c config.Log) (*slog.Logger, string, func()) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(c.Level)); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	console := slog.Handler(slog.NewTextHandler(os.Stderr, opts))
	if strings.EqualFold(c.Format, "json") {
		console = slog.NewJSONHandler(os.Stderr, opts)
	}

	dir := c.Dir
	if dir == "" {
		dir = "logs"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "log dir:", err)
		os.Exit(1)
	}
	name := filepath.Join(dir, "fomo-"+time.Now().UTC().Format("20060102T150405Z")+".jsonl")
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, "log file:", err)
		os.Exit(1)
	}
	fileLog := &lockedWriter{w: f}
	h := teeHandler{
		a: console,
		b: slog.NewJSONHandler(fileLog, opts),
	}
	closeLog := func() {
		fileLog.mu.Lock()
		_ = f.Sync()
		_ = f.Close()
		fileLog.mu.Unlock()
	}
	return slog.New(h), name, closeLog
}

// teeHandler writes each event to the console and to the JSON file.
// Record is cloned because a handler may keep it.
type teeHandler struct {
	a, b slog.Handler
}

func (t teeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return t.a.Enabled(ctx, level) || t.b.Enabled(ctx, level)
}

func (t teeHandler) Handle(ctx context.Context, r slog.Record) error {
	err1 := t.a.Handle(ctx, r.Clone())
	err2 := t.b.Handle(ctx, r.Clone())
	if err1 != nil {
		return err1
	}
	return err2
}

func (t teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return teeHandler{a: t.a.WithAttrs(attrs), b: t.b.WithAttrs(attrs)}
}

func (t teeHandler) WithGroup(name string) slog.Handler {
	return teeHandler{a: t.a.WithGroup(name), b: t.b.WithGroup(name)}
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
