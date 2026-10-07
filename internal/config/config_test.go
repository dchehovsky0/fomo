package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLoadRepoConfigWithEnv(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "123456:ABC-def")
	t.Setenv("TELEGRAM_CHAT_ID", "-1001234567890")
	t.Setenv("FOMO_ACCESS_TOKEN", "")
	t.Setenv("FOMO_CHROME_NO_SANDBOX", "")
	t.Setenv("AXIOM_REFRESH_TOKEN", "refresh")

	cfg, err := Load(filepath.Join("..", "..", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telegram.BotToken != "123456:ABC-def" || cfg.Telegram.ChatID != "-1001234567890" {
		t.Errorf("telegram = %+v", cfg.Telegram)
	}
	if cfg.Watch.Lifetime != 49*time.Hour+50*time.Minute || cfg.Watch.VolumeFor != 110*time.Minute || cfg.Watch.MinTheses != 3 || cfg.Watch.ThesisLimit != 500 || len(cfg.Watch.Schedule) != 4 || cfg.Watch.Schedule[0].Every != 2*time.Second || cfg.Watch.Schedule[0].MaxAge != 20*time.Minute || cfg.Watch.Schedule[3].Every != 2*time.Minute || cfg.Screen.After != 10*time.Second || cfg.Screen.MinVolume5m != 1000 || cfg.Screen.NoVolumeFor != 2*time.Minute || cfg.Screen.Recheck != 30*time.Second {
		t.Errorf("watch not parsed: %+v", cfg.Watch)
	}
	if ax := cfg.Axiom; ax.RefreshToken != "refresh" || len(ax.Clusters) != 4 || ax.StallTimeout != 3*time.Minute ||
		ax.ProfileDir != filepath.Join("..", "..", "axiom-profile") ||
		ax.StatsProfileDir != filepath.Join("..", "..", "axiom-stats-profile") {
		t.Errorf("axiom not parsed: %+v", ax)
	}
	last := cfg.Accounts[len(cfg.Accounts)-1]
	if len(cfg.Accounts) != 18 || cfg.Accounts[0].Name != "acc01" || last.Name != "acc18" || cfg.Accounts[0].Workers != 1 {
		t.Errorf("accounts = %d, first=%s workers=%d last=%s", len(cfg.Accounts), cfg.Accounts[0].Name, cfg.Accounts[0].Workers, last.Name)
	}
	if cfg.Accounts[0].DebugURL != "http://127.0.0.1:9221" || last.DebugURL != "http://127.0.0.1:9238" ||
		cfg.Accounts[0].ChromeProfileDir != filepath.Join("..", "..", "profiles", "acc01") ||
		last.ChromeProfileDir != filepath.Join("..", "..", "profiles", "acc18") {
		t.Errorf("attached accounts: first=%+v last=%+v", cfg.Accounts[0], last)
	}
	if cfg.Accounts[0].Proxy != "" {
		t.Errorf("attached Chrome must not take a Go proxy, got %s", cfg.Accounts[0].Proxy)
	}
	if !cfg.Session.Headless || cfg.Session.NoSandbox {
		t.Errorf("headless=%v no_sandbox=%v", cfg.Session.Headless, cfg.Session.NoSandbox)
	}
	if want := filepath.Join("..", "..", "data", "state.json"); cfg.Store.Path != want {
		t.Errorf("store path = %q, want %q (relative to config dir)", cfg.Store.Path, want)
	}
}

func TestAccountsDefaultsAndValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	os.WriteFile(path, []byte(`
session: {mode: chromedp}
accounts:
  - {name: a, chrome_profile_dir: ./p/a, proxy: "socks5://h:1080", rps: 2}
  - {chrome_profile_dir: ./p/b, workers: 4}
`), 0o600)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	a, b := cfg.Accounts[0], cfg.Accounts[1]
	if a.RPS != 2 || a.Workers != 2 || a.Proxy != "socks5://h:1080" || a.ChromeProfileDir != filepath.Join(dir, "p", "a") {
		t.Errorf("a = %+v", a)
	}
	if b.Name != "account2" || b.RPS != 1 || b.Workers != 4 {
		t.Errorf("b = %+v", b)
	}
	if got, err := cfg.Account("account2"); err != nil || got.Name != "account2" {
		t.Errorf("Account(account2) = %+v, %v", got, err)
	}
	if _, err := cfg.Account("nope"); err == nil {
		t.Error("unknown account must be an error")
	}

	os.WriteFile(path, []byte(`
session: {mode: chromedp}
accounts:
  - {name: a, chrome_profile_dir: ./p}
  - {name: a, chrome_profile_dir: ./p}
`), 0o600)
	if _, err := Load(path); err == nil {
		t.Error("duplicate account name and profile must fail validation")
	}

	os.WriteFile(path, []byte(`
session: {mode: chromedp}
axiom: {chrome_profile_dir: ./p}
accounts:
  - {name: a, chrome_profile_dir: ./p}
`), 0o600)
	if _, err := Load(path); err == nil {
		t.Error("axiom must not share a Chrome profile with an account")
	}

	os.WriteFile(path, []byte(`
session: {mode: chromedp}
axiom: {chrome_profile_dir: ./axiom-profile, stats_profile_dir: ./axiom-profile}
accounts:
  - {name: a, chrome_profile_dir: ./p}
`), 0o600)
	if _, err := Load(path); err == nil {
		t.Error("stats profile must differ from the pair profile")
	}
}

func TestAccountProxiesStayPutWhenTheRangeGrows(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	for i := 0; i < 10; i++ {
		lines = append(lines, "10.0.0."+strconv.Itoa(i+1)+":1080:user:pass")
	}
	os.WriteFile(filepath.Join(dir, "proxies.txt"), []byte(strings.Join(lines, "\n")), 0o600)
	write := func(to int) string {
		path := filepath.Join(dir, "c.yaml")
		body := `
session: {mode: chromedp}
screen: {proxies: ./proxies.txt, min_volume_5m: 1}
accounts:
  - {name: main, chrome_profile_dir: ./chrome-profile}
account_ranges:
  - {name: "acc{n}", chrome_profile_dir: "./profiles/acc{n}", from: 2, to: ` + strconv.Itoa(to) + `}
axiom: {chrome_profile_dir: ./axiom-profile}
`
		os.WriteFile(path, []byte(body), 0o600)
		return path
	}
	small, err := Load(write(25))
	if err != nil {
		t.Fatal(err)
	}
	large, err := Load(write(50))
	if err != nil {
		t.Fatal(err)
	}
	if len(small.Accounts) != 25 || len(large.Accounts) != 50 {
		t.Fatalf("accounts = %d and %d, want 25 and 50", len(small.Accounts), len(large.Accounts))
	}
	per := map[string]int{}
	for i, a := range large.Accounts {
		if a.Proxy == "" {
			t.Fatalf("account %s has no proxy", a.Name)
		}
		if i < len(small.Accounts) && a.Proxy != small.Accounts[i].Proxy {
			t.Fatalf("account %s moved proxies when the range grew", a.Name)
		}
		per[a.Proxy]++
	}
	if len(per) != 10 {
		t.Fatalf("distinct proxies = %d, want 10", len(per))
	}
	for _, n := range per {
		if n != 5 {
			t.Fatalf("accounts per proxy = %d, want 5 at 50 accounts", n)
		}
	}
}

func TestDebugURLExpandsAndMustBeHTTP(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	os.WriteFile(path, []byte(`
session: {mode: chromedp}
accounts:
  - {name: main, chrome_profile_dir: ./chrome-profile, debug_url: "http://127.0.0.1:9221"}
account_ranges:
  - {name: "acc{n}", chrome_profile_dir: "./profiles/acc{n}", debug_url: "http://127.0.0.1:92{n}", from: 2, to: 3}
axiom: {chrome_profile_dir: ./axiom-profile}
`), 0o600)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Accounts[0].DebugURL != "http://127.0.0.1:9221" || cfg.Accounts[2].DebugURL != "http://127.0.0.1:923" {
		t.Fatalf("debug urls = %q %q", cfg.Accounts[0].DebugURL, cfg.Accounts[2].DebugURL)
	}

	os.WriteFile(path, []byte(`
session: {mode: chromedp}
accounts:
  - {name: main, chrome_profile_dir: ./chrome-profile, debug_url: "9221"}
axiom: {chrome_profile_dir: ./axiom-profile}
`), 0o600)
	if _, err := Load(path); err == nil {
		t.Fatal("debug_url without a scheme must be rejected")
	}
}

func TestLoadMissingConfigFails(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("missing config must be an error, not silent defaults")
	}
}

func TestDotEnvDoesNotOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(path, []byte("# c\nFOMO_T1=\"quoted\"\nexport FOMO_T2=plain\nFOMO_T3=file\n"), 0o600)
	t.Setenv("FOMO_T3", "env")
	os.Unsetenv("FOMO_T1")
	os.Unsetenv("FOMO_T2")
	t.Cleanup(func() { os.Unsetenv("FOMO_T1"); os.Unsetenv("FOMO_T2") })

	if err := LoadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("FOMO_T1") != "quoted" || os.Getenv("FOMO_T2") != "plain" || os.Getenv("FOMO_T3") != "env" {
		t.Errorf("got %q %q %q", os.Getenv("FOMO_T1"), os.Getenv("FOMO_T2"), os.Getenv("FOMO_T3"))
	}
}
