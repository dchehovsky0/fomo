package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
)

// launchSlots limits how many browsers run at once across all accounts.
var launchSlots = make(chan struct{}, 2)

// SetChromeConcurrency sets how many Chrome instances may fetch tokens at the
// same time. Call it before any Fetch.
func SetChromeConcurrency(n int) {
	launchSlots = make(chan struct{}, max(n, 1))
}

type ChromeOptions struct {
	AccountID      string
	AppURL         string
	ProfileDir     string
	ChromePath     string
	DebugURL       string
	Headless       bool
	NoSandbox      bool
	RefreshBefore  time.Duration
	LoadTimeout    time.Duration
	RequestTimeout time.Duration
}

// Chrome reads the Fomo access token from a logged-in Chrome profile.
//
// Without DebugURL it opens a tab only when a fresh token is needed, then
// closes that tab. The browser process stays up, so a restart does not ask
// for the login again. The tab is not left on fomo.family: the site would
// poll /feed on its own and burn the shared rate limit.
//
// With DebugURL it attaches to a Chrome that is already open (the same model
// as fomo-thesis-tester) and leaves that fomo tab open. API calls for that
// account run inside the tab.
type Chrome struct {
	opts  ChromeOptions
	log   *slog.Logger
	fetch func(ctx context.Context) (string, time.Time, error)

	mu          sync.Mutex
	token       string
	exp         time.Time
	lastAttempt time.Time
	needReload  bool
	page        *pageSession
	// failErr is returned without launching Chrome until failUntil: a profile
	// without a login would otherwise hold a launch slot on every request
	// and starve the other accounts.
	failErr   error
	failUntil time.Time
}

const (
	noLoginRetry   = 10 * time.Minute
	failedRetry    = time.Minute
	usableTokenMin = 10 * time.Second
)

func NewChrome(opts ChromeOptions, log *slog.Logger) (*Chrome, error) {
	dir, err := filepath.Abs(opts.ProfileDir)
	if err != nil {
		return nil, err
	}
	opts.ProfileDir = dir
	if opts.LoadTimeout <= 0 {
		opts.LoadTimeout = 60 * time.Second
	}
	c := &Chrome{opts: opts, log: log}
	c.fetch = c.Fetch
	return c, nil
}

func (c *Chrome) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	if c.token != "" && c.exp.Sub(now) > c.opts.RefreshBefore {
		return c.token, nil
	}
	// Privy may hand back the same not-yet-expired token; don't relaunch
	// Chrome on every request while it is still usable.
	if c.token != "" && c.exp.Sub(now) > 30*time.Second && now.Sub(c.lastAttempt) < time.Minute {
		return c.token, nil
	}
	if c.failErr != nil && now.Before(c.failUntil) {
		if c.token != "" && c.exp.Sub(now) > usableTokenMin {
			return c.token, nil
		}
		return "", c.failErr
	}
	c.lastAttempt = now

	tok, exp, err := c.fetch(ctx)
	if err != nil {
		if ctx.Err() == nil {
			retry := failedRetry
			if errors.Is(err, ErrNoToken) {
				retry = noLoginRetry
			}
			c.failErr, c.failUntil = err, time.Now().Add(retry)
		}
		if c.token != "" && c.exp.Sub(now) > usableTokenMin {
			c.log.Warn("token refresh failed, using current token", "err", err, "expires_in", c.exp.Sub(now).Round(time.Second))
			return c.token, nil
		}
		return "", err
	}
	c.failErr, c.failUntil = nil, time.Time{}
	if tok != c.token {
		c.log.Info("fomo token refreshed via chrome", "expires", exp.Local().Format(time.DateTime))
	}
	c.token, c.exp = tok, exp
	return tok, nil
}

func (c *Chrome) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.exp = time.Time{}
	c.lastAttempt = time.Time{}
	c.needReload = true
}

// InPage reports that this account's API calls run inside the attached tab.
func (c *Chrome) InPage() bool {
	return strings.TrimSpace(c.opts.DebugURL) != ""
}

// Fetch opens the app in a browser and waits until localStorage holds a token
// valid for longer than RefreshBefore.
func (c *Chrome) Fetch(ctx context.Context) (tok string, exp time.Time, err error) {
	waitStart := time.Now()
	slots := launchSlots
	var slotWait time.Duration
	select {
	case slots <- struct{}{}:
		slotWait = time.Since(waitStart)
	case <-ctx.Done():
		err = ctx.Err()
		c.log.Warn("chrome token fetch cancelled", "slot_wait", time.Since(waitStart).Round(time.Millisecond), "err", err)
		return
	}
	defer func() { <-slots }()
	started := time.Now()
	defer func() {
		args := []any{
			"slot_wait", slotWait.Round(time.Millisecond),
			"took", time.Since(started).Round(time.Millisecond),
		}
		if err != nil {
			c.log.Warn("chrome token fetch failed", append(args, "err", err)...)
			return
		}
		c.log.Info("chrome token fetched", append(args, "expires_in", time.Until(exp).Round(time.Second))...)
	}()

	if c.InPage() {
		return c.fetchAttached(ctx)
	}
	return c.fetchVisit(ctx)
}

// fetchAttached keeps the user's fomo tab. Cancelling the bot does not close it.
func (c *Chrome) fetchAttached(ctx context.Context) (string, time.Time, error) {
	if c.page == nil {
		c.page = newPageSession(c.opts, c.log, true)
	}
	wasStarted := c.page.started
	if err := c.page.Start(ctx); err != nil {
		return "", time.Time{}, err
	}
	// A tab created just now already loaded the app. Reloading again is only
	// for a tab that was already open and whose token the API rejected.
	if !wasStarted && c.page.navigated {
		c.needReload = false
	}
	if err := c.reloadIfNeeded(ctx, c.page); err != nil {
		return "", time.Time{}, err
	}
	return readFreshToken(ctx, c.page, c.opts, c.log)
}

// fetchVisit opens one tab on the bot's Chrome, reads the token and closes
// that tab. The browser process stays.
func (c *Chrome) fetchVisit(ctx context.Context) (string, time.Time, error) {
	p := newPageSession(c.opts, c.log, false)
	if err := p.Start(ctx); err != nil {
		return "", time.Time{}, err
	}
	defer p.closeOwnedTab()
	// This visit just opened the app, which is the refresh.
	c.needReload = false
	return readFreshToken(ctx, p, c.opts, c.log)
}

func (c *Chrome) reloadIfNeeded(ctx context.Context, p *pageSession) error {
	if !c.needReload {
		return nil
	}
	c.needReload = false
	c.log.Info("reloading fomo so Privy can refresh the access token", "account", c.opts.AccountID)
	if err := p.Reload(ctx); err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

// FetchInPage runs one fomo API call inside the attached tab.
func (c *Chrome) FetchInPage(ctx context.Context, method, rawURL string, hdr map[string]string, body []byte) (int, []byte, map[string]string, error) {
	if !c.InPage() {
		return 0, nil, nil, errors.New("in-page fetch needs account debug_url")
	}
	c.mu.Lock()
	if c.page == nil {
		c.page = newPageSession(c.opts, c.log, true)
	}
	p := c.page
	err := p.Start(ctx)
	c.mu.Unlock()
	if err != nil {
		return 0, nil, nil, err
	}
	return p.FetchInPage(ctx, method, rawURL, hdr, body)
}

func readFreshToken(ctx context.Context, p *pageSession, opts ChromeOptions, log *slog.Logger) (string, time.Time, error) {
	runCtx, cancel := context.WithTimeout(ctx, opts.LoadTimeout)
	defer cancel()

	var (
		best     string
		bestExp  time.Time
		start    = time.Now()
		reloaded bool
	)
	for {
		tok, exp, err := p.AccessToken(runCtx)
		if err != nil && ctx.Err() != nil {
			return "", time.Time{}, ctx.Err()
		}
		if err == nil {
			if exp.After(bestExp) {
				best, bestExp = tok, exp
			}
			if time.Until(exp) > opts.RefreshBefore {
				return tok, exp, nil
			}
		}

		// The page asks Privy for a new access token on load. One reload is
		// enough when the first look only finds a token that is about to expire.
		if !reloaded && time.Since(start) > opts.LoadTimeout/3 {
			reloaded = true
			log.Info("no fresh fomo access token yet, reloading page", "account", opts.AccountID)
			if err := p.Reload(runCtx); err != nil && ctx.Err() != nil {
				return "", time.Time{}, ctx.Err()
			}
		}

		select {
		case <-runCtx.Done():
			if best != "" && time.Until(bestExp) > 30*time.Second {
				return best, bestExp, nil
			}
			if ctx.Err() != nil {
				return "", time.Time{}, ctx.Err()
			}
			return "", time.Time{}, fmt.Errorf("%w: fomo access token not found in Chrome profile %s (run: fomobot -login)",
				ErrNoToken, opts.ProfileDir)
		case <-time.After(time.Second):
		}
	}
}

// AllocatorOptions starts Chrome on opts.ProfileDir without the usual
// automation markers.
func AllocatorOptions(o ChromeOptions) []chromedp.ExecAllocatorOption {
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts,
		chromedp.UserDataDir(o.ProfileDir),
		chromedp.Flag("headless", o.Headless),
		chromedp.Flag("enable-automation", false),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.WindowSize(1280, 900),
	)
	if o.ChromePath != "" {
		opts = append(opts, chromedp.ExecPath(o.ChromePath))
	}
	if o.NoSandbox {
		opts = append(opts, chromedp.NoSandbox)
	}
	return opts
}

// HideHeadless keeps the browser's real version in the UA so it matches
// client hints and only drops the "Headless" marker.
func HideHeadless(ctx context.Context) {
	var ua string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`navigator.userAgent`, &ua)); err == nil && strings.Contains(ua, "HeadlessChrome") {
		_ = chromedp.Run(ctx, emulation.SetUserAgentOverride(strings.ReplaceAll(ua, "HeadlessChrome", "Chrome")))
	}
}

// Login starts a plain Chrome (no DevTools automation, so Google sign-in is
// not blocked) on the bot profile and waits until the user closes it.
func Login(ctx context.Context, opts ChromeOptions) error {
	path := opts.ChromePath
	if path == "" {
		path = FindChrome()
	}
	if path == "" {
		return errors.New("chrome not found, set session.chrome_path")
	}
	dir, err := filepath.Abs(opts.ProfileDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// The token check leaves a headless Chrome on this profile. A second
	// Chrome then exits at once with code 21 and no window appears.
	if err := releaseHeadless(ctx, dir); err != nil {
		return err
	}
	err = runLoginChrome(ctx, path, dir, opts.AppURL)
	if chromeExitCode(err) == chromeProfileInUse {
		if err := releaseHeadless(ctx, dir); err != nil {
			return err
		}
		err = runLoginChrome(ctx, path, dir, opts.AppURL)
	}
	if chromeExitCode(err) == chromeProfileInUse {
		return errors.New("окно Chrome этого профиля уже открыто")
	}
	return err
}

// chromeProfileInUse is Chrome's RESULT_CODE_PROFILE_IN_USE.
const chromeProfileInUse = 21

func releaseHeadless(ctx context.Context, dir string) error {
	visible, err := stopHeadlessChrome(dir)
	forgetProfile(dir)
	if err != nil {
		return err
	}
	if visible {
		return errors.New("окно Chrome этого профиля уже открыто")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Second):
		return nil
	}
}

func runLoginChrome(ctx context.Context, path, dir, appURL string) error {
	// No CommandContext: Ctrl+C stops the bot and leaves this window open.
	// No remote-debugging port: Google refuses to sign in when it is set.
	cmd := exec.Command(path,
		"--user-data-dir="+dir,
		"--no-first-run",
		"--no-default-browser-check",
		"--new-window",
		appURL,
	)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start chrome: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		if err != nil {
			return err
		}
	}
	// Chrome keeps the profile locked for a moment after the window closes.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(3 * time.Second):
		return nil
	}
}

func chromeExitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 0
}

func FindChrome() string {
	var candidates []string
	switch runtime.GOOS {
	case "windows":
		for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData"} {
			if base := os.Getenv(env); base != "" {
				candidates = append(candidates, filepath.Join(base, "Google", "Chrome", "Application", "chrome.exe"))
			}
		}
	case "darwin":
		candidates = append(candidates, "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome")
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}
