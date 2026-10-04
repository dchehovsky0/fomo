//go:build windows

package session

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// stopHeadlessChrome closes a headless Chrome that is holding dir, so a normal
// login window can open on the same profile. A visible window is left alone.
// visible reports that such a window is already open.
func stopHeadlessChrome(dir string) (visible bool, err error) {
	headless, visible, err := profileBrowsers(dir)
	if err != nil || len(headless) == 0 {
		return visible, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	args := append([]string{"/F", "/T"}, pids(headless)...)
	// taskkill exits non-zero when a child is already gone.
	_ = exec.CommandContext(ctx, "taskkill", args...).Run()
	deadline := time.Now().Add(8 * time.Second)
	for {
		headless, visible, err = profileBrowsers(dir)
		if err != nil {
			return false, err
		}
		if len(headless) == 0 {
			return visible, nil
		}
		if time.Now().After(deadline) {
			return false, errors.New("скрытый Chrome этого профиля не закрылся")
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func pids(list []int) []string {
	args := make([]string, 0, len(list)*2)
	for _, pid := range list {
		args = append(args, "/PID", fmt.Sprint(pid))
	}
	return args
}

// profileBrowsers returns browser PIDs (not renderer/gpu children) on dir.
func profileBrowsers(dir string) (headless []int, visible bool, err error) {
	script := `$dir = '` + strings.ReplaceAll(dir, "'", "''") + `'
Get-CimInstance Win32_Process -Filter "Name = 'chrome.exe'" | ForEach-Object {
  $cmd = $_.CommandLine
  if (-not $cmd) { return }
  if ($cmd.IndexOf($dir, [StringComparison]::OrdinalIgnoreCase) -lt 0) { return }
  if ($cmd -match '--type=') { return }
  if ($cmd -match '(?i)(^|\s)--headless(\s|=|$)') { 'H ' + $_.ProcessId } else { 'V ' + $_.ProcessId }
}`
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if err != nil {
		return nil, false, fmt.Errorf("list chrome: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		kind, pidText, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		var pid int
		if _, scanErr := fmt.Sscan(pidText, &pid); scanErr != nil || pid <= 0 {
			continue
		}
		switch kind {
		case "H":
			headless = append(headless, pid)
		case "V":
			visible = true
		}
	}
	return headless, visible, nil
}
