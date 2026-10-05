package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"fomobot/internal/dexscreener"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Fomo          Fomo           `yaml:"fomo"`
	Session       Session        `yaml:"session"`
	Accounts      []Account      `yaml:"accounts"`
	AccountRanges []AccountRange `yaml:"account_ranges"`
	Axiom         Axiom          `yaml:"axiom"`
	Screen        Screen         `yaml:"screen"`
	Watch         Watch          `yaml:"watch"`
	Trending      Alerts         `yaml:"trending"`
	Telegram      Telegram       `yaml:"telegram"`
	Web           Web            `yaml:"web"`
	Store         Store          `yaml:"store"`
	Log           Log            `yaml:"log"`
	Database      Database       `yaml:"database"`
	Recorder      Recorder       `yaml:"recorder"`
}

// Account is one fomo account: its own session (Chrome profile or token) and
// optionally its own proxy for API requests.
type Account struct {
	Name             string `yaml:"name"`
	ChromeProfileDir string `yaml:"chrome_profile_dir"`
	// DebugURL attaches to a Chrome already started with --remote-debugging-port,
	// for example http://127.0.0.1:9221. The fomo tab stays open and API calls
	// run inside it. Empty keeps the short-lived tab that only refreshes the token.
	DebugURL    string `yaml:"debug_url"`
	TokenFile   string `yaml:"token_file"`
	AccessToken string `yaml:"access_token"`
	// Proxy: http://user:pass@host:port or socks5://host:port; empty = direct.
	Proxy   string  `yaml:"proxy"`
	RPS     float64 `yaml:"rps"`
	Workers int     `yaml:"workers"`
}

// AccountRange expands into one account per number from From to To.
// Name, ChromeProfileDir and DebugURL may contain {n}, replaced by that number.
type AccountRange struct {
	Name             string  `yaml:"name"`
	ChromeProfileDir string  `yaml:"chrome_profile_dir"`
	DebugURL         string  `yaml:"debug_url"`
	TokenFile        string  `yaml:"token_file"`
	Proxy            string  `yaml:"proxy"`
	RPS              float64 `yaml:"rps"`
	Workers          int     `yaml:"workers"`
	From             int     `yaml:"from"`
	To               int     `yaml:"to"`
}

// Axiom is where new tokens come from: the new_pairs room of Axiom's
// WebSocket, read by a Chrome on axiom.trade. Chrome path and no_sandbox are
// taken from session.
type Axiom struct {
	AppURL     string   `yaml:"app_url"`
	Clusters   []string `yaml:"clusters"`
	ProfileDir string   `yaml:"chrome_profile_dir"`
	Headless   bool     `yaml:"headless"`
	// Cookies written into the profile when it lacks them or they changed
	// here; after that the site refreshes them itself.
	AccessToken  string `yaml:"access_token"`
	RefreshToken string `yaml:"refresh_token"`
	CFClearance  string `yaml:"cf_clearance"`
	// Protocols: only pairs of these protocols; empty = all.
	Protocols []string `yaml:"protocols"`
	// SkipProtocols never reach DexScreener, the fomo checks or alerts.
	SkipProtocols []string      `yaml:"skip_protocols"`
	StallTimeout  time.Duration `yaml:"stall_timeout"`
	StartTimeout  time.Duration `yaml:"start_timeout"`
}

// Screen asks DexScreener for market cap After the token is created.
// Below MinMarketCap the token never reaches the fomo checks. An empty
// answer is asked again and does not drop the token. Volume is not an entry filter.
// Proxies is a file of socks5://user:pass@host:port lines; each proxy checks
// 30 tokens, so 10 proxies check 300 per wave.
// Recheck asks DexScreener for volume.h24 while the token is in tiers 1–3.
// MinGrowth is the minimum fractional rise between samples (0.05 = 5%).
// Two samples in a row under that rise drop the token. See TOKEN_TIERS.md.
type Screen struct {
	After        time.Duration `yaml:"after"`
	MinMarketCap float64       `yaml:"min_market_cap"`
	Recheck      time.Duration `yaml:"recheck"`
	MinGrowth    float64       `yaml:"volume_min_growth"`
	Proxies      string        `yaml:"proxies"`
}

type Watch struct {
	Lifetime time.Duration `yaml:"lifetime"`
	// VolumeFor: DexScreener volume is checked only while the token has been
	// in tier 1 for less than this. Zero means the whole lifetime.
	VolumeFor   time.Duration `yaml:"volume_for"`
	MinTheses   int           `yaml:"min_theses"`
	ThesisLimit int           `yaml:"thesis_limit"`
	Schedule    []WatchTier   `yaml:"schedule"`
	RetryDelay  time.Duration `yaml:"retry_delay"`
	AuthPause   time.Duration `yaml:"auth_pause"`
	// RateWindow: the "theses per minute" in alerts is counted over this window.
	RateWindow time.Duration `yaml:"rate_window"`
}

// Alerts about tokens entering fomo's trending list (WebSocket trending_tokens).
type Alerts struct {
	Enabled bool   `yaml:"enabled"`
	WSURL   string `yaml:"ws_url"`
	TopicID string `yaml:"topic_id"`
	// RealertAfter: a token absent from trending longer than this is announced
	// again when it returns; 0s = once per token.
	RealertAfter        time.Duration `yaml:"realert_after"`
	SilentFirstSnapshot bool          `yaml:"silent_first_snapshot"`
}

type WatchTier struct {
	MaxAge time.Duration `yaml:"max_age"`
	Every  time.Duration `yaml:"every"`
}

type Database struct {
	URL string `yaml:"url"`
}

// Recorder configures -record mode: everything seen on fomo goes to Postgres.
type Recorder struct {
	FeedInterval       time.Duration `yaml:"feed_interval"`
	HistoryMaxPages    int           `yaml:"history_max_pages"`
	RefreshMinInterval time.Duration `yaml:"refresh_min_interval"`
	NewTokenRefresh    time.Duration `yaml:"new_token_refresh"`
	NewTokenTrackFor   time.Duration `yaml:"new_token_track_for"`
	AllTokensRefresh   time.Duration `yaml:"all_tokens_refresh"`
	TrackFor           time.Duration `yaml:"track_for"`
	SnapshotInterval   time.Duration `yaml:"snapshot_interval"`
	SnapshotBatch      int           `yaml:"snapshot_batch"`
	DetailsInterval    time.Duration `yaml:"details_interval"`
	RetryDelay         time.Duration `yaml:"retry_delay"`
	Trending           Trending      `yaml:"trending"`
}

type Trending struct {
	Enabled bool   `yaml:"enabled"`
	WSURL   string `yaml:"ws_url"`
	TopicID string `yaml:"topic_id"`
	// Sample: how often the latest trending message is stored.
	Sample time.Duration `yaml:"sample"`
	// HistoryPages for tokens first seen in trending (not in the thesis feed).
	HistoryPages int `yaml:"history_pages"`
}

type Fomo struct {
	BaseURL         string        `yaml:"base_url"`
	AppURL          string        `yaml:"app_url"`
	SupportedChains string        `yaml:"supported_chains"`
	NetworkID       string        `yaml:"network_id"`
	FeedLimit       int           `yaml:"feed_limit"`
	UserAgent       string        `yaml:"user_agent"`
	RequestTimeout  time.Duration `yaml:"request_timeout"`
	RateLimitRPS    float64       `yaml:"rate_limit_rps"`
	MaxRetries      int           `yaml:"max_retries"`
	ThesisThreshold string        `yaml:"thesis_threshold"`
}

type Session struct {
	Mode                string        `yaml:"mode"`
	AccessToken         string        `yaml:"access_token"`
	TokenFile           string        `yaml:"token_file"`
	ChromeProfileDir    string        `yaml:"chrome_profile_dir"`
	ChromePath          string        `yaml:"chrome_path"`
	Headless            bool          `yaml:"headless"`
	NoSandbox           bool          `yaml:"no_sandbox"`
	RefreshBeforeExpiry time.Duration `yaml:"refresh_before_expiry"`
	PageLoadTimeout     time.Duration `yaml:"page_load_timeout"`
	// ChromeConcurrency caps headless Chrome instances refreshing tokens at once.
	ChromeConcurrency int `yaml:"chrome_concurrency"`
}

type Telegram struct {
	BotToken          string `yaml:"bot_token"`
	ChatID            string `yaml:"chat_id"`
	TokenLinkTemplate string `yaml:"token_link_template"`
	AxiomLinkTemplate string `yaml:"axiom_link_template"`
	// Timezone of thesis times in alerts and on theses pages.
	Timezone string `yaml:"timezone"`
}

// Web serves the pages with full theses texts linked from alerts.
type Web struct {
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen"`
	// PublicURL: how Telegram users reach this server; without it alerts go
	// without the theses button.
	PublicURL string        `yaml:"public_url"`
	PagesDir  string        `yaml:"pages_dir"`
	Keep      time.Duration `yaml:"keep"`
	// AdminToken protects /health/*, the watch board and /api/board when public_url is set.
	AdminToken string `yaml:"admin_token"`
}

type Store struct {
	Path          string        `yaml:"path"`
	FlushInterval time.Duration `yaml:"flush_interval"`
}

type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
	// Dir receives one JSON line per event, next to the console log.
	Dir string `yaml:"dir"`
}

func Default() Config {
	return Config{
		Fomo: Fomo{
			BaseURL:         "https://prod-api.fomo.family",
			AppURL:          "https://fomo.family/",
			SupportedChains: "1,56,143,4663,5042,8453,1399811149",
			NetworkID:       "1399811149",
			FeedLimit:       50,
			UserAgent:       "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36",
			RequestTimeout:  10 * time.Second,
			RateLimitRPS:    1,
			MaxRetries:      5,
			ThesisThreshold: "0",
		},
		Session: Session{
			Mode:                "static",
			ChromeProfileDir:    "./chrome-profile",
			Headless:            true,
			RefreshBeforeExpiry: 5 * time.Minute,
			PageLoadTimeout:     60 * time.Second,
			ChromeConcurrency:   2,
		},
		Axiom: Axiom{
			AppURL: "https://axiom.trade/",
			Clusters: []string{
				"wss://cluster9.axiom.trade/", "wss://cluster3.axiom.trade/",
				"wss://cluster5.axiom.trade/", "wss://cluster7.axiom.trade/",
			},
			ProfileDir:    "./axiom-profile",
			StallTimeout:  3 * time.Minute,
			StartTimeout:  90 * time.Second,
			SkipProtocols: []string{"Meteora AMM V2", "Meteora DLMM", "Raydium CLMM", "Orca"},
		},
		Screen: Screen{
			After:        10 * time.Second,
			MinMarketCap: 7_000,
			Recheck:      30 * time.Second,
			MinGrowth:    0.05,
			Proxies:      "./proxies.txt",
		},
		Watch: Watch{
			Lifetime:    49*time.Hour + 50*time.Minute,
			VolumeFor:   110 * time.Minute,
			MinTheses:   3,
			ThesisLimit: 500,
			Schedule: []WatchTier{
				{MaxAge: 20 * time.Minute, Every: 2 * time.Second},
				{MaxAge: 50 * time.Minute, Every: 5 * time.Second},
				{MaxAge: 110 * time.Minute, Every: 30 * time.Second},
				{MaxAge: 49*time.Hour + 50*time.Minute, Every: 2 * time.Minute},
			},
			RetryDelay: 30 * time.Second,
			AuthPause:  time.Minute,
			RateWindow: 10 * time.Minute,
		},
		Web: Web{
			Enabled:  true,
			Listen:   ":8080",
			PagesDir: "./data/pages",
			Keep:     7 * 24 * time.Hour,
		},
		Trending: Alerts{
			Enabled:             true,
			WSURL:               "wss://prod-api.fomo.family/ws",
			TopicID:             "1,56,143,4663,5042,8453,1399811149",
			RealertAfter:        24 * time.Hour,
			SilentFirstSnapshot: true,
		},
		Telegram: Telegram{
			TokenLinkTemplate: "https://fomo.family/tokens/solana/{mint}",
			AxiomLinkTemplate: "https://axiom.trade/meme/{pair}?chain=sol",
			Timezone:          "Europe/Moscow",
		},
		Store: Store{
			Path:          "./data/state.json",
			FlushInterval: 30 * time.Second,
		},
		Log: Log{Level: "info", Format: "text", Dir: "./logs"},
		Recorder: Recorder{
			FeedInterval:       4 * time.Second,
			HistoryMaxPages:    10,
			RefreshMinInterval: 10 * time.Second,
			NewTokenRefresh:    30 * time.Second,
			NewTokenTrackFor:   2 * time.Hour,
			AllTokensRefresh:   15 * time.Minute,
			TrackFor:           6 * time.Hour,
			SnapshotInterval:   time.Minute,
			SnapshotBatch:      50,
			DetailsInterval:    30 * time.Minute,
			RetryDelay:         30 * time.Second,
			Trending: Trending{
				Enabled:      true,
				WSURL:        "wss://prod-api.fomo.family/ws",
				TopicID:      "1,56,143,4663,5042,8453,1399811149",
				Sample:       30 * time.Second,
				HistoryPages: 1,
			},
		},
	}
}

// Load reads the YAML config, expanding ${VAR} references from the environment.
// Relative file paths in the config are resolved against the config's directory,
// so the bot behaves the same whatever the working directory is.
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal([]byte(os.ExpandEnv(string(data))), &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := cfg.expandAccounts(); err != nil {
		return cfg, err
	}
	if len(cfg.Accounts) == 0 {
		cfg.Accounts = []Account{{
			Name: "main", ChromeProfileDir: cfg.Session.ChromeProfileDir,
			TokenFile: cfg.Session.TokenFile, AccessToken: cfg.Session.AccessToken,
		}}
	}
	if cfg.Log.Dir == "" {
		cfg.Log.Dir = "./logs"
	}
	dir := filepath.Dir(path)
	paths := []*string{&cfg.Session.TokenFile, &cfg.Session.ChromeProfileDir, &cfg.Axiom.ProfileDir, &cfg.Screen.Proxies, &cfg.Store.Path, &cfg.Web.PagesDir, &cfg.Log.Dir}
	for i := range cfg.Accounts {
		paths = append(paths, &cfg.Accounts[i].ChromeProfileDir, &cfg.Accounts[i].TokenFile)
	}
	for _, p := range paths {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(dir, *p)
		}
	}
	if err := cfg.fillAccountProxies(); err != nil {
		return cfg, err
	}
	return cfg, cfg.Validate()
}

// fillAccountProxies pins every account with an empty proxy to a line of
// screen.proxies, round-robin by its position in the expanded list. An
// account that sets proxy itself is left alone and still occupies its slot,
// so raising an account_range's `to` does not move the accounts already there.
// A missing proxy file leaves those accounts direct.
func (c *Config) fillAccountProxies() error {
	need := false
	for i := range c.Accounts {
		if c.Accounts[i].Proxy == "" && c.Accounts[i].DebugURL == "" {
			need = true
			break
		}
	}
	if !need || c.Screen.Proxies == "" {
		return nil
	}
	proxies, err := dexscreener.LoadProxies(c.Screen.Proxies)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if len(proxies) == 0 {
		return nil
	}
	for i := range c.Accounts {
		// An attached Chrome sends fomo requests from the page, not through the Go proxy.
		if c.Accounts[i].Proxy == "" && c.Accounts[i].DebugURL == "" {
			c.Accounts[i].Proxy = proxies[i%len(proxies)]
		}
	}
	return nil
}

func (c *Config) expandAccounts() error {
	for _, r := range c.AccountRanges {
		if r.From < 1 || r.To < r.From || r.To-r.From > 500 {
			return fmt.Errorf("account_ranges %q: from/to must span 1..500 accounts, got %d..%d", r.Name, r.From, r.To)
		}
		for n := r.From; n <= r.To; n++ {
			repl := strings.NewReplacer("{n}", strconv.Itoa(n))
			c.Accounts = append(c.Accounts, Account{
				Name: repl.Replace(r.Name), ChromeProfileDir: repl.Replace(r.ChromeProfileDir),
				DebugURL:  repl.Replace(r.DebugURL),
				TokenFile: repl.Replace(r.TokenFile), Proxy: r.Proxy, RPS: r.RPS, Workers: r.Workers,
			})
		}
	}
	return nil
}

// Location is telegram.timezone, the machine's zone when empty.
func (c *Config) Location() (*time.Location, error) {
	if c.Telegram.Timezone == "" {
		return time.Local, nil
	}
	return time.LoadLocation(c.Telegram.Timezone)
}

// Account returns the account with the given name; an empty name means the first one.
func (c *Config) Account(name string) (Account, error) {
	if len(c.Accounts) == 0 {
		return Account{}, errors.New("no accounts configured")
	}
	if name == "" {
		return c.Accounts[0], nil
	}
	var names []string
	for _, a := range c.Accounts {
		if a.Name == name {
			return a, nil
		}
		names = append(names, a.Name)
	}
	return Account{}, fmt.Errorf("unknown account %q, configured: %s", name, strings.Join(names, ", "))
}

func (c *Config) Validate() error {
	var errs []error
	switch c.Session.Mode {
	case "static", "chromedp":
	default:
		errs = append(errs, fmt.Errorf("session.mode must be static or chromedp, got %q", c.Session.Mode))
	}
	if c.Session.ChromeConcurrency < 1 {
		c.Session.ChromeConcurrency = 1
	}
	if c.Fomo.FeedLimit < 1 {
		errs = append(errs, errors.New("fomo.feed_limit must be >= 1"))
	}
	if c.Fomo.RateLimitRPS <= 0 {
		errs = append(errs, errors.New("fomo.rate_limit_rps must be > 0"))
	}

	names, profiles := map[string]bool{}, map[string]bool{}
	for i := range c.Accounts {
		a := &c.Accounts[i]
		if a.Name == "" {
			a.Name = fmt.Sprintf("account%d", i+1)
		}
		if names[a.Name] {
			errs = append(errs, fmt.Errorf("accounts: duplicate name %q", a.Name))
		}
		names[a.Name] = true
		if a.RPS <= 0 {
			a.RPS = c.Fomo.RateLimitRPS
		}
		if a.Workers < 1 {
			a.Workers = 2
		}
		if c.Session.Mode == "chromedp" {
			if a.DebugURL != "" && !strings.HasPrefix(a.DebugURL, "http://") && !strings.HasPrefix(a.DebugURL, "https://") {
				errs = append(errs, fmt.Errorf("accounts[%s]: debug_url must be http://host:port", a.Name))
			}
			if a.ChromeProfileDir == "" {
				errs = append(errs, fmt.Errorf("accounts[%s]: chrome_profile_dir is required in chromedp mode", a.Name))
			} else if profiles[a.ChromeProfileDir] {
				errs = append(errs, fmt.Errorf("accounts[%s]: chrome_profile_dir %s is used by another account", a.Name, a.ChromeProfileDir))
			}
			profiles[a.ChromeProfileDir] = true
		}
	}

	ax := &c.Axiom
	if ax.AppURL == "" || len(ax.Clusters) == 0 {
		errs = append(errs, errors.New("axiom.app_url and axiom.clusters are required"))
	}
	if ax.ProfileDir == "" {
		errs = append(errs, errors.New("axiom.chrome_profile_dir is required"))
	}
	for _, a := range c.Accounts {
		if a.ChromeProfileDir != "" && filepath.Clean(a.ChromeProfileDir) == filepath.Clean(ax.ProfileDir) {
			errs = append(errs, fmt.Errorf("axiom.chrome_profile_dir %s is used by account %s", ax.ProfileDir, a.Name))
		}
	}
	if ax.StallTimeout <= 0 || ax.StartTimeout <= 0 {
		errs = append(errs, errors.New("axiom.stall_timeout and start_timeout must be > 0"))
	}

	w := &c.Watch
	if w.Lifetime <= 0 {
		errs = append(errs, errors.New("watch.lifetime must be > 0"))
	}
	if w.MinTheses < 1 {
		errs = append(errs, errors.New("watch.min_theses must be >= 1"))
	}
	if w.ThesisLimit < w.MinTheses {
		w.ThesisLimit = max(w.MinTheses, 500)
	}
	if len(w.Schedule) == 0 {
		errs = append(errs, errors.New("watch.schedule must have at least one tier"))
	}
	for i, t := range w.Schedule {
		if t.Every <= 0 || t.MaxAge <= 0 {
			errs = append(errs, fmt.Errorf("watch.schedule[%d]: max_age and every must be > 0", i))
		}
	}

	if w.RateWindow <= 0 {
		w.RateWindow = 10 * time.Minute
	}
	if _, err := c.Location(); err != nil {
		errs = append(errs, fmt.Errorf("telegram.timezone: %w", err))
	}
	if c.Web.Enabled {
		if c.Web.Listen == "" || c.Web.PagesDir == "" {
			errs = append(errs, errors.New("web.listen and web.pages_dir are required when web.enabled"))
		}
		if u := c.Web.PublicURL; u != "" && !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			errs = append(errs, fmt.Errorf("web.public_url must start with http:// or https://, got %q", u))
		}
	}
	if c.Trending.Enabled && c.Trending.WSURL == "" {
		errs = append(errs, errors.New("trending.ws_url is required when trending.enabled"))
	}
	if c.Trending.RealertAfter < 0 {
		errs = append(errs, errors.New("trending.realert_after must be >= 0"))
	}

	r := &c.Recorder
	if r.FeedInterval <= 0 {
		errs = append(errs, errors.New("recorder.feed_interval must be > 0"))
	}
	if r.SnapshotInterval <= 0 {
		errs = append(errs, errors.New("recorder.snapshot_interval must be > 0"))
	}
	r.HistoryMaxPages = max(r.HistoryMaxPages, 1)
	r.Trending.HistoryPages = max(r.Trending.HistoryPages, 1)
	if r.SnapshotBatch < 1 {
		r.SnapshotBatch = 50
	}
	if r.Trending.Sample <= 0 {
		r.Trending.Sample = 30 * time.Second
	}
	if r.RetryDelay <= 0 {
		r.RetryDelay = 30 * time.Second
	}
	return errors.Join(errs...)
}

// LoadDotEnv sets variables from a KEY=VALUE file without overriding ones
// already present in the environment. A missing file is ignored.
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' && val[len(val)-1] == '"' || val[0] == '\'' && val[len(val)-1] == '\'') {
			val = val[1 : len(val)-1]
		}
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
	return sc.Err()
}
