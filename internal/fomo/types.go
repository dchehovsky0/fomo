package fomo

import (
	"encoding/json"
	"fmt"
	"time"
)

const (
	TypeThesisCreated = "thesis_created" // event type in /feed
	TypeThesis        = "thesis"         // item type in /feed/token/thesis
	SolanaNetworkID   = "1399811149"
)

type Envelope[T any] struct {
	Success        *bool  `json:"success"`
	Message        string `json:"message"`
	ResponseObject T      `json:"responseObject"`
	StatusCode     int    `json:"statusCode"`
}

// Thesis is the format-independent view of a thesis used by the rest of the
// bot. The two endpoints return different shapes, see Event and TokenThesisItem.
type Thesis struct {
	ID           string
	UserID       string
	Handle       string
	TokenAddress string
	NetworkID    string
	Ticker       string
	CreatedAt    time.Time
	Verified     bool
	IsDev        bool
	// PositionUSD is the author's current position in the token.
	PositionUSD    float64
	PositionClosed bool
	// MarketCap is the current market cap; only the global feed carries it.
	MarketCap           float64
	MarketCapAtCreation float64
	Comment             string
	AuthorImageURL      string
	TokenImageURL       string
}

// AuthorKey identifies an author for "unique authors" counting.
func (t Thesis) AuthorKey() string {
	if t.UserID != "" {
		return t.UserID
	}
	return t.Handle
}

// Event is an item of the global feed (/feed). Thesis details live in Body.
// Its id is the feed event id; the thesis id is Body.CommentID.
type Event struct {
	ID           FlexString      `json:"id"`
	Type         string          `json:"type"`
	UserID       FlexString      `json:"userId"`
	TradeID      FlexString      `json:"tradeId"`
	SwapID       FlexString      `json:"swapId"`
	TransferID   FlexString      `json:"transferId"`
	TokenAddress string          `json:"tokenAddress"`
	NetworkID    FlexString      `json:"networkId"`
	CreatedAt    FlexTime        `json:"createdAt"`
	Verified     FlexBool        `json:"verified"`
	Twitter      json.RawMessage `json:"twitter"`
	Likes        FlexInt         `json:"likes"`
	Views        FlexInt         `json:"views"`
	NumReplies   FlexInt         `json:"numReplies"`
	Pinned       FlexBool        `json:"pinned"`
	Body         EventBody       `json:"body"`
	TradeComment *ThesisComment  `json:"tradeComment"`

	// Raw is the original JSON of the event, kept for the theses log.
	Raw json.RawMessage `json:"-"`
}

type EventBody struct {
	Ticker              string    `json:"ticker"`
	Comment             string    `json:"comment"`
	CommentID           string    `json:"commentId"`
	UserHandle          string    `json:"userHandle"`
	DisplayName         string    `json:"displayName"`
	IsDev               FlexBool  `json:"isDev"`
	PositionNotionalUSD FlexFloat `json:"positionNotionalUsd"`
	HumanTokenAmount    FlexFloat `json:"humanTokenAmount"`
	MarketCap           FlexFloat `json:"marketCap"`
	Price               FlexFloat `json:"price"`
	FDV                 FlexFloat `json:"fdv"`
	MarketCapAtCreation FlexFloat `json:"marketCapAtCreation"`
	PriceUSDAtCreation  FlexFloat `json:"priceUsdAtCreation"`
	FDVAtCreation       FlexFloat `json:"fdvAtCreation"`
	RealizedPnlUSD      FlexFloat `json:"realizedPnlUsd"`
	UnrealizedPnlUSD    FlexFloat `json:"unrealizedPnlUsd"`
	PctRealizedPnl      FlexFloat `json:"percentageRealizedPnl"`
	PctUnrealizedPnl    FlexFloat `json:"percentageUnrealizedPnl"`
	UserImageURL        string    `json:"userImageUrl"`
	TokenImageURL       string    `json:"tokenImageUrl"`
}

func (e *Event) IsThesis() bool { return e.Type == TypeThesisCreated }

// ThesisID is the thesis id (t2-...), shared with /feed/token/thesis.
func (e *Event) ThesisID() string {
	if e.Body.CommentID != "" {
		return e.Body.CommentID
	}
	if e.TradeComment != nil {
		return e.TradeComment.ID.String()
	}
	return ""
}

func (e *Event) Thesis() Thesis {
	return Thesis{
		ID:                  e.ID.String(),
		UserID:              e.UserID.String(),
		Handle:              firstNonEmpty(e.Body.UserHandle, e.Body.DisplayName, e.UserID.String()),
		TokenAddress:        e.TokenAddress,
		NetworkID:           e.NetworkID.String(),
		Ticker:              e.Body.Ticker,
		CreatedAt:           e.CreatedAt.Time,
		Verified:            bool(e.Verified),
		IsDev:               bool(e.Body.IsDev),
		PositionUSD:         e.Body.PositionNotionalUSD.Float(),
		MarketCap:           e.Body.MarketCap.Float(),
		MarketCapAtCreation: e.Body.MarketCapAtCreation.Float(),
		Comment:             e.Body.Comment,
		AuthorImageURL:      e.Body.UserImageURL,
		TokenImageURL:       e.Body.TokenImageURL,
	}
}

// TokenThesisItem is an item of /feed/token/thesis: a flat object with
// type "thesis", no body, the position in authorTrade.usdValue.
type TokenThesisItem struct {
	ID            FlexString      `json:"id"`
	Type          string          `json:"type"`
	TradeID       FlexString      `json:"tradeId"`
	CreatedAt     FlexTime        `json:"createdAt"`
	UserID        FlexString      `json:"userId"`
	DisplayName   string          `json:"displayName"`
	UserHandle    string          `json:"userHandle"`
	Verified      FlexBool        `json:"verified"`
	IsDev         FlexBool        `json:"isDev"`
	Comment       ThesisComment   `json:"comment"`
	AuthorTrade   *AuthorTrade    `json:"authorTrade"`
	NumReplies    FlexInt         `json:"numReplies"`
	TokenAddress  string          `json:"tokenAddress"`
	NetworkID     FlexString      `json:"networkId"`
	Ticker        string          `json:"ticker"`
	TokenImageURL string          `json:"tokenImageUrl"`
	UserImageURL  string          `json:"profilePictureLink"`
	Equity        FlexFloat       `json:"equity"`
	Threshold     FlexFloat       `json:"threshold"`
	Twitter       json.RawMessage `json:"twitter"`
}

type ThesisComment struct {
	ID                  FlexString `json:"id"`
	Comment             string     `json:"comment"`
	PriceUSDAtCreation  FlexFloat  `json:"priceUsdAtCreation"`
	MarketCapAtCreation FlexFloat  `json:"marketCapAtCreation"`
	FDVAtCreation       FlexFloat  `json:"fdvAtCreation"`
	CreatedAt           FlexTime   `json:"createdAt"`
	NumLikes            FlexInt    `json:"numLikes"`
	OlderThesis         FlexInt    `json:"olderThesis"`
	NewerThesis         FlexInt    `json:"newerThesis"`
}

// UnmarshalJSON also accepts a bare string, in case the comment is not
// expanded into an object.
func (c *ThesisComment) UnmarshalJSON(b []byte) error {
	*c = ThesisComment{}
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &c.Comment)
	}
	if string(b) == "null" {
		return nil
	}
	type plain ThesisComment
	return json.Unmarshal(b, (*plain)(c))
}

type AuthorTrade struct {
	HumanTokenAmount FlexFloat `json:"humanTokenAmount"`
	USDValue         FlexFloat `json:"usdValue"`
	UnrealizedPnlUSD FlexFloat `json:"unrealizedPnlUsd"`
	RealizedPnlUSD   FlexFloat `json:"realizedPnlUsd"`
	PctUnrealizedPnl FlexFloat `json:"percentageUnrealizedPnl"`
	PctRealizedPnl   FlexFloat `json:"percentageRealizedPnl"`
	ClosedAt         FlexTime  `json:"closedAt"`
}

func (it *TokenThesisItem) Thesis() Thesis {
	t := Thesis{
		ID:                  it.ID.String(),
		UserID:              it.UserID.String(),
		Handle:              firstNonEmpty(it.UserHandle, it.DisplayName, it.UserID.String()),
		TokenAddress:        it.TokenAddress,
		NetworkID:           it.NetworkID.String(),
		Ticker:              it.Ticker,
		CreatedAt:           it.CreatedAt.Time,
		Verified:            bool(it.Verified),
		IsDev:               bool(it.IsDev),
		MarketCapAtCreation: it.Comment.MarketCapAtCreation.Float(),
		Comment:             it.Comment.Comment,
		AuthorImageURL:      it.UserImageURL,
		TokenImageURL:       it.TokenImageURL,
	}
	if it.AuthorTrade != nil {
		t.PositionUSD = it.AuthorTrade.USDValue.Float()
		t.PositionClosed = !it.AuthorTrade.ClosedAt.IsZero()
	}
	return t
}

type TokenThesisPage struct {
	Items []Thesis
	// Raw holds the decoded items with their original JSON, aligned with Items.
	Raw         []RawTokenThesis
	HasNextPage bool
	Count       int
	// LastID is the id of the last raw item, the cursor for the next page.
	LastID string
}

// Observed is how many theses this response proves. sortedThesis writes a new
// thesis into items immediately and updates count later: count stays at 2, or
// even 0, while items already holds the third thesis. Count can also be the
// total outside this page, so it wins when it is the larger number.
func (p *TokenThesisPage) Observed() int {
	if p == nil {
		return 0
	}
	if n := len(p.Items); p.Count > n {
		return p.Count
	}
	return len(p.Items)
}

type RawTokenThesis struct {
	Item TokenThesisItem
	JSON json.RawMessage
}

type feedResponse struct {
	Feed []json.RawMessage `json:"feed"`
}

type tokenThesisResponse struct {
	Items       []json.RawMessage `json:"items"`
	HasNextPage FlexBool          `json:"hasNextPage"`
	Count       FlexInt           `json:"count"`
}

func decodeEnvelope[T any](body []byte) (T, error) {
	var env Envelope[T]
	if err := json.Unmarshal(body, &env); err != nil {
		var zero T
		return zero, fmt.Errorf("decode response: %w", err)
	}
	if env.Success != nil && !*env.Success {
		return env.ResponseObject, fmt.Errorf("fomo api error: %s (statusCode %d)", env.Message, env.StatusCode)
	}
	return env.ResponseObject, nil
}

type itemHead struct {
	ID   FlexString `json:"id"`
	Type string     `json:"type"`
}

// ParseFeed decodes a /feed response. Events are decoded one by one so that a
// malformed item is skipped instead of failing the whole response. Only
// undecodable theses are reported: other event types are dropped anyway.
func ParseFeed(body []byte) ([]Event, []error, error) {
	resp, err := decodeEnvelope[feedResponse](body)
	if err != nil {
		return nil, nil, err
	}
	events := make([]Event, 0, len(resp.Feed))
	var errs []error
	for i, raw := range resp.Feed {
		var e Event
		if err := json.Unmarshal(raw, &e); err != nil {
			var head itemHead
			if json.Unmarshal(raw, &head) != nil || head.Type == TypeThesisCreated {
				errs = append(errs, fmt.Errorf("feed event #%d (id %q): %w", i, head.ID, err))
			}
			continue
		}
		e.Raw = raw
		events = append(events, e)
	}
	return events, errs, nil
}

// ParseTokenThesis decodes a /feed/token/thesis response. Items of types other
// than "thesis" are skipped (they still count for LastID).
func ParseTokenThesis(body []byte) (*TokenThesisPage, []error, error) {
	resp, err := decodeEnvelope[tokenThesisResponse](body)
	if err != nil {
		return nil, nil, err
	}
	page := &TokenThesisPage{HasNextPage: bool(resp.HasNextPage), Count: int(resp.Count)}
	var errs []error
	for i, raw := range resp.Items {
		var head itemHead
		_ = json.Unmarshal(raw, &head)
		if head.ID != "" {
			page.LastID = head.ID.String()
		}
		if head.Type != "" && head.Type != TypeThesis {
			continue
		}
		var it TokenThesisItem
		if err := json.Unmarshal(raw, &it); err != nil {
			errs = append(errs, fmt.Errorf("token thesis #%d (id %q): %w", i, head.ID, err))
			continue
		}
		page.Items = append(page.Items, it.Thesis())
		page.Raw = append(page.Raw, RawTokenThesis{Item: it, JSON: raw})
	}
	return page, errs, nil
}

// responseObject extracts responseObject from the standard envelope; a body
// without the envelope is returned as is.
func responseObject(body []byte) (json.RawMessage, error) {
	if !json.Valid(body) {
		return nil, fmt.Errorf("decode response: invalid JSON: %s", snippet(body))
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal(body, &probe) != nil {
		return json.RawMessage(body), nil // not an object, e.g. a bare array
	}
	if _, ok := probe["responseObject"]; !ok {
		return json.RawMessage(body), nil
	}
	return decodeEnvelope[json.RawMessage](body)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
