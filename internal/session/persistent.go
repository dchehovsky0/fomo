package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
)

// profileSlot is one long-lived Chrome for a profile directory. The bot
// attaches and detaches; stopping the bot does not close the window, so the
// login is still there on the next start.
type profileSlot struct {
	mu     sync.Mutex
	remote context.Context
}

var profileSlots sync.Map // absolute profile dir -> *profileSlot

// forgetProfile drops the cached connection after that Chrome process was closed.
func forgetProfile(dir string) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return
	}
	profileSlots.Delete(abs)
}

// OpenTab opens a tab in the profile's Chrome, starting that Chrome only when
// it is not already running. closeTab closes the tab. The browser process is
// left running.
func OpenTab(ctx context.Context, opts ChromeOptions) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	remote, err := remoteAllocator(opts)
	if err != nil {
		return nil, nil, err
	}
	tabCtx, cancelTab := chromedp.NewContext(remote)
	var once sync.Once
	closeTab := func() { once.Do(cancelTab) }
	go func() {
		select {
		case <-ctx.Done():
			closeTab()
		case <-tabCtx.Done():
		}
	}()
	return tabCtx, closeTab, nil
}

func remoteAllocator(opts ChromeOptions) (context.Context, error) {
	dir, err := filepath.Abs(opts.ProfileDir)
	if err != nil {
		return nil, err
	}
	opts.ProfileDir = dir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	v, _ := profileSlots.LoadOrStore(dir, &profileSlot{})
	slot := v.(*profileSlot)
	slot.mu.Lock()
	defer slot.mu.Unlock()

	if slot.remote != nil {
		if _, ok := devtoolsURL(dir); ok {
			return slot.remote, nil
		}
		slot.remote = nil
	}
	if url, ok := devtoolsURL(dir); ok {
		slot.remote, _ = chromedp.NewRemoteAllocator(context.Background(), url)
		return slot.remote, nil
	}

	// Background, not the bot context: cancelling the bot must not kill Chrome.
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), AllocatorOptions(opts)...)
	boot, _ := chromedp.NewContext(allocCtx)
	if err := chromedp.Run(boot); err != nil {
		cancelAlloc()
		return nil, fmt.Errorf("start chrome: %w", err)
	}
	url, err := waitDevtools(dir)
	if err != nil {
		cancelAlloc()
		return nil, err
	}
	slot.remote, _ = chromedp.NewRemoteAllocator(context.Background(), url)
	return slot.remote, nil
}

func waitDevtools(dir string) (string, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		if url, ok := devtoolsURL(dir); ok {
			return url, nil
		}
		if time.Now().After(deadline) {
			return "", errors.New("chrome started but the debugging port did not open")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func devtoolsURL(dir string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "DevToolsActivePort"))
	if err != nil {
		return "", false
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	port := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
	if port == "" || strings.IndexFunc(port, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return "", false
	}
	url := "http://127.0.0.1:" + port
	client := &http.Client{Timeout: time.Second}
	for range 2 {
		resp, err := client.Get(url + "/json/version")
		if resp != nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		if err == nil && resp != nil && resp.StatusCode == http.StatusOK {
			return url, true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return "", false
}
