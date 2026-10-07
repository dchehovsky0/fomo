// Package proxy reads the proxy list assigned to fomo accounts.
package proxy

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	"golang.org/x/net/proxy"
)

// Load reads proxy lines. A line is either a URL
// (socks5://user:pass@host:port or http://...) or host:port:user:pass, which
// is treated as SOCKS5. Empty lines and lines starting with # are skipped.
func Load(path string) ([]string, error) {
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
		u, err := parse(line)
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

func parse(line string) (string, error) {
	if strings.Contains(line, "://") {
		u, err := url.Parse(line)
		if err != nil || u.Host == "" {
			return "", fmt.Errorf("bad proxy url")
		}
		return u.String(), nil
	}
	host, port, user, pass, ok := split(line)
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

func split(line string) (host, port, user, pass string, ok bool) {
	parts := strings.SplitN(line, ":", 4)
	if len(parts) != 4 || parts[0] == "" || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		return "", "", "", "", false
	}
	return parts[0], parts[1], parts[2], parts[3], true
}

// Transport dials through a proxy URL. socks5:// is SOCKS5; anything else
// with a host is an HTTP proxy.
func Transport(raw string) (*http.Transport, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("proxy: bad url")
	}
	tr := &http.Transport{}
	switch u.Scheme {
	case "socks5", "socks5h":
		d, err := proxy.FromURL(u, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("proxy: %w", err)
		}
		cd, ok := d.(proxy.ContextDialer)
		if !ok {
			tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return d.Dial(network, addr)
			}
			return tr, nil
		}
		tr.DialContext = cd.DialContext
	default:
		tr.Proxy = http.ProxyURL(u)
	}
	return tr, nil
}

var proxyUser = regexp.MustCompile(`//[^/\s@]+@`)

// Redact strips user:password from a proxy URL inside an error string.
func Redact(err error) string {
	if err == nil {
		return ""
	}
	return proxyUser.ReplaceAllString(err.Error(), "//")
}
