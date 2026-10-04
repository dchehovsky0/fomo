package dexscreener

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRedactProxyErr(t *testing.T) {
	got := redactProxyErr(errors.New(`Get "https://api.dexscreener.com/x": proxy http://user:secret@10.0.0.1:8080: refused`))
	if strings.Contains(got, "secret") || strings.Contains(got, "user:") {
		t.Fatalf("password leaked: %s", got)
	}
}

func TestParseProxy(t *testing.T) {
	u, err := parseProxy("10.0.0.1:8080:user:secret")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u, "socks5://user:secret@10.0.0.1:8080") {
		t.Fatalf("url = %s", u)
	}
	if _, err := parseProxy("nope"); err == nil {
		t.Fatal("expected error")
	}
}

func TestTokensBatch(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"dexId":"pump","pairAddress":"p1","marketCap":4000,"fdv":9000,"baseToken":{"address":"mintA"}},
			{"dexId":"raydium","pairAddress":"p2","marketCap":0,"fdv":20000,"liquidity":{"usd":5},"baseToken":{"address":"mintB"}},
			{"dexId":"raydium","pairAddress":"p3","marketCap":50000,"liquidity":{"usd":90},"baseToken":{"address":"mintB"}}
		]`))
	}))
	defer srv.Close()
	c := New(time.Second)
	c.BaseURL = srv.URL
	got, err := c.Tokens(context.Background(), []string{"mintA", "mintB"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(path, "/tokens/v1/solana/mintA,mintB") {
		t.Fatalf("path = %s", path)
	}
	if got["mintA"].MarketCap != 4000 {
		t.Fatalf("mintA = %+v", got["mintA"])
	}
	if got["mintB"].MarketCap != 50000 || got["mintB"].PairAddress != "p3" {
		t.Fatalf("mintB picked the thin pair: %+v", got["mintB"])
	}
}

func TestTokensPrefersCapWhenLiquidityTies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[
			{"pairAddress":"empty","marketCap":0,"fdv":0,"baseToken":{"address":"mint"}},
			{"pairAddress":"real","marketCap":0,"fdv":8000,"baseToken":{"address":"mint"}}
		]`))
	}))
	defer srv.Close()
	c := New(time.Second)
	c.BaseURL = srv.URL
	got, err := c.Tokens(context.Background(), []string{"mint"})
	if err != nil {
		t.Fatal(err)
	}
	if got["mint"].PairAddress != "real" || got["mint"].MarketCap != 8000 {
		t.Fatalf("got %+v", got["mint"])
	}
}

func TestTokensPrefersPricedPoolOverDeeperEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[
			{"pairAddress":"deep","liquidity":{"usd":100},"marketCap":0,"baseToken":{"address":"mint"}},
			{"pairAddress":"priced","liquidity":{"usd":1},"marketCap":8000,"baseToken":{"address":"mint"}}
		]`))
	}))
	defer srv.Close()
	c := New(time.Second)
	c.BaseURL = srv.URL
	got, err := c.Tokens(context.Background(), []string{"mint"})
	if err != nil {
		t.Fatal(err)
	}
	if got["mint"].PairAddress != "priced" || got["mint"].MarketCap != 8000 {
		t.Fatalf("got %+v", got["mint"])
	}
}

func TestTokensSumsVolumeAcrossPools(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[
			{"pairAddress":"a","marketCap":1000,"liquidity":{"usd":1},"volume":{"h24":2000},"baseToken":{"address":"mint"}},
			{"pairAddress":"b","marketCap":9000,"liquidity":{"usd":50},"volume":{"h24":5000},"baseToken":{"address":"mint"}}
		]`))
	}))
	defer srv.Close()
	c := New(time.Second)
	c.BaseURL = srv.URL
	got, err := c.Tokens(context.Background(), []string{"mint"})
	if err != nil {
		t.Fatal(err)
	}
	if got["mint"].PairAddress != "b" || got["mint"].MarketCap != 9000 || got["mint"].VolumeUSD != 7000 {
		t.Fatalf("got %+v", got["mint"])
	}
}

func TestPoolWaveSplitsAcrossClients(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`[{"marketCap":12000,"baseToken":{"address":"only"}}]`))
	}))
	defer srv.Close()
	mk := func() *Client {
		c := New(time.Second)
		c.BaseURL = srv.URL
		return c
	}
	p := &Pool{clients: []*Client{mk(), mk()}}
	if p.Limit() != 60 {
		t.Fatalf("limit = %d", p.Limit())
	}
	mints := make([]string, 35)
	for i := range mints {
		mints[i] = "m"
	}
	mints[0] = "only"
	quotes, failed, err := p.Wave(context.Background(), mints)
	if err != nil || len(failed) != 0 || quotes["only"].MarketCap != 12000 || hits.Load() != 2 {
		t.Fatalf("quotes=%v failed=%v hits=%d err=%v", quotes, failed, hits.Load(), err)
	}
}
