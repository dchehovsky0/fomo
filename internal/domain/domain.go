// Package domain holds the bot's own data: a new token and the alert built
// about it. Adapters (Axiom, fomo, Telegram, the pages) convert to and from
// these types; nothing here knows about them.
package domain

import "strings"
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
	// Taken from the launch message and kept until the token is watched.
	Deployer     string
	Website      string
	Twitter      string
	Telegram     string
	Discord      string
	ImageURL     string
	LiquiditySOL float64
	// EntryVolume is the pair's volume at the moment the filter let the token in.
	EntryVolume Volume
}

// Volume is trading on the token's pair in Axiom's 5-minute and 1-hour
// windows: buys plus sells in USD, and how many trades that was.
type Volume struct {
	USD5m    float64 `json:"volume_5m"`
	USD1h    float64 `json:"volume_1h"`
	Trades5m int     `json:"trades_5m"`
	Trades1h int     `json:"trades_1h"`
}

// AxiomImage is the token picture Axiom keeps for a mint. The launch message
// often leaves token_image empty, while this file is already there.
func AxiomImage(mint string) string {
	if !base58Mint(mint) {
		return ""
	}
	return "https://axiomtrading.sfo3.cdn.digitaloceanspaces.com/" + mint + ".webp"
}

func base58Mint(s string) bool {
	if len(s) < 32 || len(s) > 44 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '1' && r <= '9':
		case r >= 'A' && r <= 'H', r >= 'J' && r <= 'N', r >= 'P' && r <= 'Z':
		case r >= 'a' && r <= 'k', r >= 'm' && r <= 'z':
		default:
			return false
		}
	}
	return true
}

// PictureURL is an image address the board can load. ipfs:// becomes an
// https gateway. Anything that is not http(s) or ipfs is dropped.
func PictureURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "ipfs://") {
		rest := strings.TrimPrefix(raw, "ipfs://")
		rest = strings.TrimPrefix(rest, "ipfs/")
		if rest == "" {
			return ""
		}
		return "https://ipfs.io/ipfs/" + rest
	}
	if strings.HasPrefix(raw, "https://") || strings.HasPrefix(raw, "http://") {
		return raw
	}
	return ""
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
	// Doubled is a growth alert: the thesis count hit the next doubling
	// after the first alert (6, 12, 24, …).
	Doubled  bool `json:"doubled,omitempty"`
	Rank     int  `json:"rank,omitempty"`
	Returned bool `json:"returned,omitempty"`

	Token    string `json:"token"`
	Symbol   string `json:"symbol"`
	Name     string `json:"name"`
	Chain    string `json:"chain"`
	ImageURL string `json:"image_url,omitempty"`
	// Volume is the pair's last volume sample before the alert, taken at VolumeAt.
	Volume   Volume    `json:"volume,omitzero"`
	VolumeAt time.Time `json:"volume_at,omitzero"`

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
	// Latest is the newest theses at alert time, oldest first, at most as
	// many as First. The feed shows these.
	Latest []Thesis `json:"latest,omitempty"`
	// Theses is every thesis fetched for the alert, oldest first. The open
	// token page sorts this list. Empty on alerts saved before that page.
	Theses []Thesis `json:"theses,omitempty"`
	// Tier is the watch tier (1–4) when a theses alert was sent.
	Tier int `json:"tier,omitempty"`
	// FomoMS is how long the fomo request behind this alert took.
	FomoMS int64 `json:"fomo_ms,omitempty"`

	CreatedAt  time.Time `json:"created_at,omitzero"`
	Dex        string    `json:"dex,omitempty"`
	DetectedAt time.Time `json:"detected_at"`
	// SentAt is when the alert text was assembled and handed to Telegram.
	SentAt time.Time `json:"sent_at,omitzero"`

	FomoURL  string `json:"fomo_url,omitempty"`
	AxiomURL string `json:"axiom_url,omitempty"`
	PageURL  string `json:"-"`
}
