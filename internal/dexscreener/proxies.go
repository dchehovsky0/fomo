package dexscreener

import (
	"bufio"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

// LoadProxies reads proxy lines. A line is either a URL
// (socks5://user:pass@host:port or http://...) or host:port:user:pass, which
// is treated as SOCKS5. Empty lines and lines starting with # are skipped.
func LoadProxies(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("proxies: %w", err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		u, err := parseProxy(line)
		if err != nil {
			return nil, fmt.Errorf("proxies:%d: %w", lineNo, err)
		}
		out = append(out, u)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("proxies: %w", err)
	}
	return out, nil
}

func parseProxy(line string) (string, error) {
	if strings.Contains(line, "://") {
		u, err := url.Parse(line)
		if err != nil || u.Host == "" {
			return "", fmt.Errorf("bad proxy url")
		}
		return u.String(), nil
	}
	host, port, user, pass, ok := splitProxy(line)
	if !ok {
		return "", fmt.Errorf("want host:port:user:pass")
	}
	u := &url.URL{
		Scheme: "socks5",
		Host:   net.JoinHostPort(host, port),
		User:   url.UserPassword(user, pass),
	}
	return u.String(), nil
}

func splitProxy(line string) (host, port, user, pass string, ok bool) {
	parts := strings.SplitN(line, ":", 4)
	if len(parts) != 4 || parts[0] == "" || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		return "", "", "", "", false
	}
	return parts[0], parts[1], parts[2], parts[3], true
}
