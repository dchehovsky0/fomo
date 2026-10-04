// Package dexscreener reads token pairs from the public DexScreener API
// (no key, about 300 requests per minute).
package dexscreener

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Pair struct {
	DexID        string
	PairAddress  string
	LiquidityUSD float64
	MarketCap    float64
	// VolumeUSD is volume.h24. On a one-minute-old pair that is the volume
	// since the pool appeared. VolumeKnown is false when DexScreener sent no
	// volume object; a reported 0 is known.
	VolumeUSD   float64
	VolumeKnown bool
	TokenName   string
	TokenSymbol string
}

// Quote is the market cap and volume used to screen a mint.
type Quote struct {
	MarketCap   float64
	VolumeUSD   float64
	VolumeKnown bool
}

type Client struct {
	BaseURL string
	httpc   *http.Client
}

func New(timeout time.Duration) *Client {
	return &Client{BaseURL: "https://api.dexscreener.com", httpc: &http.Client{Timeout: timeout}}
}

type apiPair struct {
	DexID       string `json:"dexId"`
	PairAddress string `json:"pairAddress"`
	Liquidity   *struct {
		USD float64 `json:"usd"`
	} `json:"liquidity"`
	MarketCap float64 `json:"marketCap"`
	FDV       float64 `json:"fdv"`
	Volume    *struct {
		H24 float64 `json:"h24"`
	} `json:"volume"`
	BaseToken struct {
		Address string `json:"address"`
		Name    string `json:"name"`
		Symbol  string `json:"symbol"`
	} `json:"baseToken"`
}

// Pairs returns the Solana pairs of a token as listed by DexScreener.
func (c *Client) Pairs(ctx context.Context, mint string) ([]Pair, error) {
	u := c.BaseURL + "/token-pairs/v1/solana/" + url.PathEscape(mint)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dexscreener: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("dexscreener: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dexscreener: HTTP %d", resp.StatusCode)
	}
	var raw []apiPair
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("dexscreener: decode: %w", err)
	}
	pairs := make([]Pair, 0, len(raw))
	for _, p := range raw {
		pair := pairFromAPI(p)
		if p.BaseToken.Address == mint {
			pair.TokenName, pair.TokenSymbol = p.BaseToken.Name, p.BaseToken.Symbol
		}
		pairs = append(pairs, pair)
	}
	return pairs, nil
}

// Tokens asks DexScreener for up to 30 Solana mints in one request
// (GET /tokens/v1/solana/a,b,c). The returned map is the best pair of each
// mint that came back; a mint with no pair is absent.
func (c *Client) Tokens(ctx context.Context, mints []string) (map[string]Pair, error) {
	if len(mints) == 0 {
		return map[string]Pair{}, nil
	}
	u := c.BaseURL + "/tokens/v1/solana/" + strings.Join(mints, ",")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dexscreener: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("dexscreener: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dexscreener: HTTP %d", resp.StatusCode)
	}
	var raw []apiPair
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("dexscreener: decode: %w", err)
	}
	out := map[string]Pair{}
	for _, p := range raw {
		mint := p.BaseToken.Address
		if mint == "" {
			continue
		}
		pair := pairFromAPI(p)
		if prev, ok := out[mint]; ok {
			// Total volume is the sum of the pools that reported one.
			pair.VolumeUSD += prev.VolumeUSD
			pair.VolumeKnown = pair.VolumeKnown || prev.VolumeKnown
			if !betterCap(pair, prev) {
				prev.VolumeUSD = pair.VolumeUSD
				prev.VolumeKnown = pair.VolumeKnown
				out[mint] = prev
				continue
			}
		}
		out[mint] = pair
	}
	return out, nil
}

func pairFromAPI(p apiPair) Pair {
	pair := Pair{DexID: p.DexID, PairAddress: p.PairAddress, MarketCap: p.MarketCap}
	if pair.MarketCap == 0 {
		pair.MarketCap = p.FDV
	}
	if p.Liquidity != nil {
		pair.LiquidityUSD = p.Liquidity.USD
	}
	if p.Volume != nil {
		pair.VolumeUSD = p.Volume.H24
		pair.VolumeKnown = true
	}
	return pair
}

// betterCap keeps a pool that actually has a market cap over a deeper pool
// that reports none (wrapped SOL does this). Otherwise the deeper pool wins,
// and equal liquidity keeps the higher cap.
func betterCap(pair, prev Pair) bool {
	if (pair.MarketCap > 0) != (prev.MarketCap > 0) {
		return pair.MarketCap > 0
	}
	if pair.LiquidityUSD != prev.LiquidityUSD {
		return pair.LiquidityUSD > prev.LiquidityUSD
	}
	return pair.MarketCap > prev.MarketCap
}

// Best is the pair with the most liquidity; with no liquidity data (a
// pump.fun bonding curve) the first listed pair.
func Best(pairs []Pair) (Pair, bool) {
	if len(pairs) == 0 {
		return Pair{}, false
	}
	best := pairs[0]
	for _, p := range pairs[1:] {
		if p.LiquidityUSD > best.LiquidityUSD {
			best = p
		}
	}
	return best, true
}
