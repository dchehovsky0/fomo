package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

type Account struct {
	ID         string `json:"id"`
	ProfileDir string `json:"profile_dir"`
	DebugURL   string `json:"debug_url"`
}

type Raw struct {
	RunDuration           string    `json:"run_duration"`
	PollInterval          string    `json:"poll_interval"`
	PerAccountMinInterval string    `json:"per_account_min_interval"`
	AuthCooldown          string    `json:"auth_cooldown"`
	RateLimitCooldown     string    `json:"rate_limit_cooldown"`
	MaxRateLimitCooldown  string    `json:"max_rate_limit_cooldown"`
	RequestTimeout        string    `json:"request_timeout"`
	BrowserReadyTimeout   string    `json:"browser_ready_timeout"`
	ThesisLookback        string    `json:"thesis_lookback"`
	AuditLookback         string    `json:"audit_lookback"`
	AuditEvery            int       `json:"audit_every"`
	ThesisLimit           int       `json:"thesis_limit"`
	ThesisThreshold       int       `json:"thesis_threshold"`
	FomoAppURL            string    `json:"fomo_app_url"`
	ChromePath            string    `json:"chrome_path"`
	Headless              bool      `json:"headless"`
	LogDir                string    `json:"log_dir"`
	Accounts              []Account `json:"accounts"`
}

type Config struct {
	RunDuration           time.Duration
	PollInterval          time.Duration
	PerAccountMinInterval time.Duration
	AuthCooldown          time.Duration
	RateLimitCooldown     time.Duration
	MaxRateLimitCooldown  time.Duration
	RequestTimeout        time.Duration
	BrowserReadyTimeout   time.Duration
	ThesisLookback        time.Duration
	AuditLookback         time.Duration
	AuditEvery            int
	ThesisLimit           int
	ThesisThreshold       int
	FomoAppURL            string
	ChromePath            string
	Headless              bool
	LogDir                string
	Accounts              []Account
}

type Token struct {
	Address   string `json:"address"`
	NetworkID int64  `json:"network_id"`
}

func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var r Raw
	if err := json.Unmarshal(b, &r); err != nil {
		return Config{}, err
	}

	parseRequired := func(name, s string) (time.Duration, error) {
		d, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", name, err)
		}
		return d, nil
	}
	parseOptional := func(name, s string, fallback time.Duration) (time.Duration, error) {
		if s == "" {
			return fallback, nil
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", name, err)
		}
		return d, nil
	}

	var c Config
	if c.RunDuration, err = parseRequired("run_duration", r.RunDuration); err != nil {
		return Config{}, err
	}
	if c.PollInterval, err = parseRequired("poll_interval", r.PollInterval); err != nil {
		return Config{}, err
	}
	if c.PerAccountMinInterval, err = parseRequired("per_account_min_interval", r.PerAccountMinInterval); err != nil {
		return Config{}, err
	}
	if c.AuthCooldown, err = parseRequired("auth_cooldown", r.AuthCooldown); err != nil {
		return Config{}, err
	}
	if c.RateLimitCooldown, err = parseRequired("rate_limit_cooldown", r.RateLimitCooldown); err != nil {
		return Config{}, err
	}
	if c.MaxRateLimitCooldown, err = parseRequired("max_rate_limit_cooldown", r.MaxRateLimitCooldown); err != nil {
		return Config{}, err
	}
	if c.RequestTimeout, err = parseRequired("request_timeout", r.RequestTimeout); err != nil {
		return Config{}, err
	}
	if c.BrowserReadyTimeout, err = parseRequired("browser_ready_timeout", r.BrowserReadyTimeout); err != nil {
		return Config{}, err
	}
	if c.ThesisLookback, err = parseOptional("thesis_lookback", r.ThesisLookback, 5*time.Minute); err != nil {
		return Config{}, err
	}
	if c.AuditLookback, err = parseOptional("audit_lookback", r.AuditLookback, 30*time.Minute); err != nil {
		return Config{}, err
	}

	c.AuditEvery = r.AuditEvery
	if c.AuditEvery == 0 {
		c.AuditEvery = 10
	}
	c.ThesisLimit = r.ThesisLimit
	if c.ThesisLimit == 0 {
		c.ThesisLimit = 500
	}
	c.ThesisThreshold = r.ThesisThreshold
	c.FomoAppURL = r.FomoAppURL
	c.ChromePath = r.ChromePath
	c.Headless = r.Headless
	c.LogDir = r.LogDir
	c.Accounts = r.Accounts

	if len(c.Accounts) == 0 {
		return Config{}, fmt.Errorf("no accounts configured")
	}
	if c.PollInterval <= 0 {
		return Config{}, fmt.Errorf("poll_interval must be > 0")
	}
	if c.PerAccountMinInterval <= 0 {
		return Config{}, fmt.Errorf("per_account_min_interval must be > 0")
	}
	if c.ThesisLookback <= 0 || c.AuditLookback <= 0 {
		return Config{}, fmt.Errorf("thesis lookbacks must be > 0")
	}
	if c.AuditEvery < 0 {
		return Config{}, fmt.Errorf("audit_every must be >= 0")
	}
	if c.ThesisLimit <= 0 || c.ThesisLimit > 500 {
		return Config{}, fmt.Errorf("thesis_limit must be between 1 and 500")
	}
	if c.FomoAppURL == "" {
		c.FomoAppURL = "https://fomo.family/token"
	}
	if c.LogDir == "" {
		c.LogDir = "./logs"
	}
	return c, nil
}

func LoadTokens(path string) ([]Token, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tokens []Token
	if err := json.Unmarshal(b, &tokens); err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return nil, fmt.Errorf("no tokens configured")
	}
	seen := make(map[string]struct{}, len(tokens))
	for i, t := range tokens {
		if t.Address == "" || t.NetworkID == 0 {
			return nil, fmt.Errorf("invalid token at index %d", i)
		}
		key := fmt.Sprintf("%d|%s", t.NetworkID, t.Address)
		if _, ok := seen[key]; ok {
			return nil, fmt.Errorf("duplicate token at index %d: %s", i, t.Address)
		}
		seen[key] = struct{}{}
	}
	return tokens, nil
}
