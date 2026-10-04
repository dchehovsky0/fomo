package dexscreener

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

const batchSize = 30

// Pool spreads token lookups across one HTTP client per proxy. Each client
// sends at most one batch of 30 mints per Wave, so 10 proxies check 300 tokens.
type Pool struct {
	clients []*Client
}

// NewPool builds a client per proxy URL. socks5:// is dialled as SOCKS5;
// http:// is an HTTP proxy. An empty list is one direct client.
func NewPool(timeout time.Duration, proxies []string) (*Pool, error) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if len(proxies) == 0 {
		return &Pool{clients: []*Client{New(timeout)}}, nil
	}
	clients := make([]*Client, 0, len(proxies))
	for _, raw := range proxies {
		tr, err := proxyTransport(raw)
		if err != nil {
			return nil, err
		}
		clients = append(clients, &Client{
			BaseURL: "https://api.dexscreener.com",
			httpc:   &http.Client{Timeout: timeout, Transport: tr},
		})
	}
	return &Pool{clients: clients}, nil
}

func proxyTransport(raw string) (*http.Transport, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("dexscreener proxy: bad url")
	}
	tr := &http.Transport{}
	switch u.Scheme {
	case "socks5", "socks5h":
		d, err := proxy.FromURL(u, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("dexscreener proxy: %w", err)
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

// Limit is how many mints one Wave sends: one batch of 30 per proxy.
func (p *Pool) Limit() int {
	n := len(p.clients) * batchSize
	if n < batchSize {
		return batchSize
	}
	return n
}

// Wave fetches market cap and volume.h24. quotes holds mints DexScreener
// returned. failed holds mints whose request did not complete; they were not
// judged. err is that failure with proxy passwords removed.
func (p *Pool) Wave(ctx context.Context, mints []string) (quotes map[string]Quote, failed []string, err error) {
	quotes = map[string]Quote{}
	if len(mints) == 0 {
		return quotes, nil, nil
	}
	limit := p.Limit()
	if len(mints) > limit {
		mints = mints[:limit]
	}
	batches := chunk(mints, batchSize)
	var mu sync.Mutex
	var wg sync.WaitGroup
	var errs []string
	for i, batch := range batches {
		wg.Add(1)
		go func(client *Client, batch []string) {
			defer wg.Done()
			got, callErr := client.Tokens(ctx, batch)
			mu.Lock()
			defer mu.Unlock()
			if callErr != nil {
				failed = append(failed, batch...)
				if len(errs) < 3 {
					errs = append(errs, redactProxyErr(callErr))
				}
				return
			}
			for mint, pair := range got {
				quotes[mint] = Quote{MarketCap: pair.MarketCap, VolumeUSD: pair.VolumeUSD, VolumeKnown: pair.VolumeKnown}
			}
		}(p.clients[i%len(p.clients)], batch)
	}
	wg.Wait()
	if len(errs) > 0 {
		err = fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return quotes, failed, err
}

var proxyUser = regexp.MustCompile(`//[^/\s@]+@`)

func redactProxyErr(err error) string {
	if err == nil {
		return ""
	}
	return proxyUser.ReplaceAllString(err.Error(), "//")
}

func chunk(mints []string, n int) [][]string {
	var out [][]string
	for len(mints) > 0 {
		if len(mints) < n {
			n = len(mints)
		}
		out = append(out, mints[:n:n])
		mints = mints[n:]
	}
	return out
}
