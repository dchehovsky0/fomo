// Package domain holds the bot's own data: a new token and the alert built
// about it. Adapters (Axiom, fomo, Telegram, the pages) convert to and from
// these types; nothing here knows about them.
package domain

import "time"

const (
	KindTheses   = "theses"   // a young token reached the thesis threshold
	KindTrending = "trending" // a token entered fomo's trending list
)

// Token is a new Solana token taken from the launch stream.
type Token struct {
	Mint      string
	CreatedAt time.Time
	// Dex is the protocol as the source shows it, e.g. "Pump V1".
	Dex    string
	Pool   string
	Symbol string
	Name   string
}

// Thesis is one thesis as shown in alerts and on the theses page.
type Thesis struct {
	At          time.Time `json:"at"`
	Handle      string    `json:"handle"`
	AvatarURL   string    `json:"avatar_url,omitempty"`
	Verified    bool      `json:"verified,omitempty"`
	IsDev       bool      `json:"is_dev,omitempty"`
	Closed      bool      `json:"closed,omitempty"`
	PositionUSD float64   `json:"position_usd"`
	MarketCap   float64   `json:"market_cap,omitempty"` // at the time of the thesis
	Text        string    `json:"text"`
}

// Alert is everything shown to the user, before it is rendered.
type Alert struct {
	Kind      string `json:"kind"`
	Threshold int    `json:"threshold,omitempty"`
	Rank      int    `json:"rank,omitempty"`
	Returned  bool   `json:"returned,omitempty"`

	Token     string  `json:"token"`
	Symbol    string  `json:"symbol"`
	Name      string  `json:"name"`
	Chain     string  `json:"chain"`
	ImageURL  string  `json:"image_url,omitempty"`
	MarketCap float64 `json:"market_cap"`

	CountKnown bool `json:"count_known"`
	Count      int  `json:"count"`
	// Recent theses within RateWindow before DetectedAt; RecentCapped means
	// there were at least Recent.
	RateKnown    bool          `json:"rate_known"`
	Recent       int           `json:"recent"`
	RecentCapped bool          `json:"recent_capped,omitempty"`
	RateWindow   time.Duration `json:"rate_window"`

	// First theses in time order; FirstExact is false when only a part of the
	// theses could be fetched and these are the earliest of that part.
	First      []Thesis `json:"first"`
	FirstExact bool     `json:"first_exact"`

	CreatedAt  time.Time `json:"created_at,omitzero"`
	Dex        string    `json:"dex,omitempty"`
	DetectedAt time.Time `json:"detected_at"`
	// SentAt is when the alert text was assembled and handed to Telegram.
	SentAt time.Time `json:"sent_at,omitzero"`

	FomoURL  string `json:"fomo_url,omitempty"`
	AxiomURL string `json:"axiom_url,omitempty"`
	PageURL  string `json:"-"`
}
