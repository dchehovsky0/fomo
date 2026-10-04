// Package db stores everything the recorder sees on fomo in PostgreSQL.
package db

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

type DB struct {
	pool *pgxpool.Pool
}

func Open(ctx context.Context, url string) (*DB, error) {
	if url == "" {
		return nil, errors.New("database.url is empty (set DATABASE_URL)")
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	d := &DB{pool: pool}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return d, nil
}

func (d *DB) Close() { d.pool.Close() }

func (d *DB) StartRun(ctx context.Context, at time.Time, config any) (int64, error) {
	var id int64
	err := d.pool.QueryRow(ctx,
		`INSERT INTO runs (started_at, config) VALUES ($1, $2) RETURNING id`,
		at, jsonOrNil(config)).Scan(&id)
	return id, err
}

func (d *DB) StopRun(ctx context.Context, id int64, at time.Time) error {
	_, err := d.pool.Exec(ctx, `UPDATE runs SET stopped_at = $2 WHERE id = $1`, id, at)
	return err
}

// Token is a sighting of a token.
type Token struct {
	Address   string
	NetworkID string
	Ticker    string
	ImageURL  string
	Source    string
	RunID     int64
	At        time.Time
}

// UpsertToken records a token; it reports whether the token is new to the database.
func (d *DB) UpsertToken(ctx context.Context, t Token) (bool, error) {
	var inserted bool
	err := d.pool.QueryRow(ctx, `
INSERT INTO tokens (token_address, network_id, ticker, image_url, first_seen_at, first_seen_source,
                    first_seen_run, last_activity_at, updated_at)
VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), $5, $6, $7, $5, $5)
ON CONFLICT (token_address) DO UPDATE SET
    ticker           = COALESCE(tokens.ticker, EXCLUDED.ticker),
    image_url        = COALESCE(tokens.image_url, EXCLUDED.image_url),
    last_activity_at = GREATEST(tokens.last_activity_at, EXCLUDED.last_activity_at),
    updated_at       = EXCLUDED.updated_at
RETURNING (xmax = 0)`,
		t.Address, t.NetworkID, clean(t.Ticker), t.ImageURL, t.At, t.Source, nullID(t.RunID)).Scan(&inserted)
	return inserted, err
}

// MarkFeedThesis notes that a thesis for the token appeared in the live feed.
func (d *DB) MarkFeedThesis(ctx context.Context, mint string, at time.Time) error {
	_, err := d.pool.Exec(ctx, `
UPDATE tokens SET first_feed_thesis_at = COALESCE(first_feed_thesis_at, $2),
                  last_activity_at = GREATEST(last_activity_at, $2), updated_at = $2
WHERE token_address = $1`, mint, at)
	return err
}

// Count is a /feed/token/thesis count observation.
type Count struct {
	Token       string
	At          time.Time
	Count       int
	HasNextPage bool
	Reason      string
	// FirstThesisAt is the oldest thesis seen by this check; Complete means
	// the whole list was read, so it is the token's first thesis.
	FirstThesisAt time.Time
	Complete      bool
}

func (d *DB) SaveCount(ctx context.Context, c Count) error {
	return pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
INSERT INTO thesis_counts (token_address, observed_at, count, has_next_page, reason)
VALUES ($1, $2, $3, $4, $5)`, c.Token, c.At, c.Count, c.HasNextPage, c.Reason); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
UPDATE tokens SET
    thesis_count              = $2,
    thesis_count_at_discovery = COALESCE(thesis_count_at_discovery, $2),
    count_checked_at          = $3,
    first_thesis_at           = LEAST(first_thesis_at, $4::timestamptz),
    history_complete          = history_complete OR $5,
    updated_at                = $3
WHERE token_address = $1`, c.Token, c.Count, c.At, nullTime(c.FirstThesisAt), c.Complete)
		return err
	})
}

// Thesis is one observation of a thesis. Pointer fields are only set when the
// source carries them, so merging two sources never erases known values.
type Thesis struct {
	ID           string
	TokenAddress string
	NetworkID    string
	Ticker       string
	UserID       string
	Handle       string
	DisplayName  string
	UserImageURL string
	Verified     bool
	IsDev        bool
	Twitter      json.RawMessage
	CreatedAt    time.Time
	TradeID      string
	SwapID       string
	TransferID   string
	FeedEventID  string
	Comment      string

	PriceAtCreation     float64
	MarketCapAtCreation float64
	FDVAtCreation       float64

	PositionUSD      *float64
	HumanTokenAmount *float64
	RealizedPnl      *float64
	UnrealizedPnl    *float64
	PctRealizedPnl   *float64
	PctUnrealizedPnl *float64
	ClosedAt         *time.Time
	Equity           *float64
	Threshold        *float64
	Likes            *int
	Views            *int
	NumReplies       *int
	OlderThesis      *int
	NewerThesis      *int
	TokenMarketCap   *float64
	TokenPrice       *float64
	TokenFDV         *float64

	Source     string // feed | token_thesis
	ObservedAt time.Time
	Raw        json.RawMessage
}

const (
	SourceFeed        = "feed"
	SourceTokenThesis = "token_thesis"
)

// SaveTheses upserts theses and their authors and appends one observation per thesis.
func (d *DB) SaveTheses(ctx context.Context, list []Thesis) error {
	if len(list) == 0 {
		return nil
	}
	b := &pgx.Batch{}
	for _, t := range list {
		raw := cleanJSON(t.Raw)
		var rawFeed, rawToken json.RawMessage
		var seenInFeed *time.Time
		if t.Source == SourceFeed {
			rawFeed = raw
			seenInFeed = &t.ObservedAt
		} else {
			rawToken = raw
		}
		b.Queue(`
INSERT INTO theses (thesis_id, token_address, network_id, ticker, user_id, user_handle, display_name,
    verified, is_dev, twitter, created_at, trade_id, swap_id, transfer_id, feed_event_id, comment,
    price_usd_at_creation, market_cap_at_creation, fdv_at_creation, first_seen_at, first_seen_source,
    seen_in_feed_at, position_usd, human_token_amount, realized_pnl_usd, unrealized_pnl_usd,
    pct_realized_pnl, pct_unrealized_pnl, position_closed_at, equity, threshold, likes, views,
    num_replies, older_thesis, newer_thesis, token_market_cap, token_price, token_fdv, updated_at,
    raw_feed, raw_token)
VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), NULLIF($6, ''), NULLIF($7, ''), $8, $9, $10, $11,
    NULLIF($12, ''), NULLIF($13, ''), NULLIF($14, ''), NULLIF($15, ''), $16, $17, $18, $19, $20, $21,
    $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33, $34, $35, $36, $37, $38, $39, $20,
    $40, $41)
ON CONFLICT (thesis_id) DO UPDATE SET
    ticker             = COALESCE(EXCLUDED.ticker, theses.ticker),
    user_handle        = COALESCE(EXCLUDED.user_handle, theses.user_handle),
    display_name       = COALESCE(EXCLUDED.display_name, theses.display_name),
    verified           = EXCLUDED.verified,
    twitter            = COALESCE(EXCLUDED.twitter, theses.twitter),
    trade_id           = COALESCE(theses.trade_id, EXCLUDED.trade_id),
    swap_id            = COALESCE(theses.swap_id, EXCLUDED.swap_id),
    transfer_id        = COALESCE(theses.transfer_id, EXCLUDED.transfer_id),
    feed_event_id      = COALESCE(theses.feed_event_id, EXCLUDED.feed_event_id),
    seen_in_feed_at    = COALESCE(theses.seen_in_feed_at, EXCLUDED.seen_in_feed_at),
    position_usd       = COALESCE(EXCLUDED.position_usd, theses.position_usd),
    human_token_amount = COALESCE(EXCLUDED.human_token_amount, theses.human_token_amount),
    realized_pnl_usd   = COALESCE(EXCLUDED.realized_pnl_usd, theses.realized_pnl_usd),
    unrealized_pnl_usd = COALESCE(EXCLUDED.unrealized_pnl_usd, theses.unrealized_pnl_usd),
    pct_realized_pnl   = COALESCE(EXCLUDED.pct_realized_pnl, theses.pct_realized_pnl),
    pct_unrealized_pnl = COALESCE(EXCLUDED.pct_unrealized_pnl, theses.pct_unrealized_pnl),
    position_closed_at = CASE WHEN EXCLUDED.first_seen_source = 'token_thesis'
                              THEN EXCLUDED.position_closed_at ELSE theses.position_closed_at END,
    equity             = COALESCE(EXCLUDED.equity, theses.equity),
    threshold          = COALESCE(EXCLUDED.threshold, theses.threshold),
    likes              = COALESCE(EXCLUDED.likes, theses.likes),
    views              = COALESCE(EXCLUDED.views, theses.views),
    num_replies        = COALESCE(EXCLUDED.num_replies, theses.num_replies),
    older_thesis       = COALESCE(EXCLUDED.older_thesis, theses.older_thesis),
    newer_thesis       = COALESCE(EXCLUDED.newer_thesis, theses.newer_thesis),
    token_market_cap   = COALESCE(EXCLUDED.token_market_cap, theses.token_market_cap),
    token_price        = COALESCE(EXCLUDED.token_price, theses.token_price),
    token_fdv          = COALESCE(EXCLUDED.token_fdv, theses.token_fdv),
    updated_at         = EXCLUDED.updated_at,
    raw_feed           = COALESCE(EXCLUDED.raw_feed, theses.raw_feed),
    raw_token          = COALESCE(EXCLUDED.raw_token, theses.raw_token)`,
			t.ID, t.TokenAddress, t.NetworkID, clean(t.Ticker), t.UserID, clean(t.Handle), clean(t.DisplayName),
			t.Verified, t.IsDev, jsonOrNil(t.Twitter), t.CreatedAt, t.TradeID, t.SwapID, t.TransferID,
			t.FeedEventID, clean(t.Comment), t.PriceAtCreation, t.MarketCapAtCreation, t.FDVAtCreation,
			t.ObservedAt, t.Source, seenInFeed, t.PositionUSD, t.HumanTokenAmount, t.RealizedPnl,
			t.UnrealizedPnl, t.PctRealizedPnl, t.PctUnrealizedPnl, t.ClosedAt, t.Equity, t.Threshold,
			t.Likes, t.Views, t.NumReplies, t.OlderThesis, t.NewerThesis, t.TokenMarketCap, t.TokenPrice,
			t.TokenFDV, rawFeed, rawToken)
		b.Queue(`
INSERT INTO thesis_observations (thesis_id, token_address, observed_at, source, position_usd,
    human_token_amount, realized_pnl_usd, unrealized_pnl_usd, position_closed_at, token_market_cap,
    token_price, likes, views, num_replies, raw)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
			t.ID, t.TokenAddress, t.ObservedAt, t.Source, t.PositionUSD, t.HumanTokenAmount, t.RealizedPnl,
			t.UnrealizedPnl, t.ClosedAt, t.TokenMarketCap, t.TokenPrice, t.Likes, t.Views, t.NumReplies, raw)
		if t.UserID != "" {
			b.Queue(`
INSERT INTO users (user_id, handle, display_name, verified, twitter, image_url, first_seen_at, last_seen_at)
VALUES ($1, NULLIF($2, ''), NULLIF($3, ''), $4, $5, NULLIF($6, ''), $7, $7)
ON CONFLICT (user_id) DO UPDATE SET
    handle       = COALESCE(EXCLUDED.handle, users.handle),
    display_name = COALESCE(EXCLUDED.display_name, users.display_name),
    verified     = EXCLUDED.verified,
    twitter      = COALESCE(EXCLUDED.twitter, users.twitter),
    image_url    = COALESCE(EXCLUDED.image_url, users.image_url),
    last_seen_at = GREATEST(users.last_seen_at, EXCLUDED.last_seen_at)`,
				t.UserID, clean(t.Handle), clean(t.DisplayName), t.Verified, jsonOrNil(t.Twitter),
				t.UserImageURL, t.ObservedAt)
		}
	}
	return d.sendBatch(ctx, b)
}

// FeedEvent is a raw /feed event.
type FeedEvent struct {
	ID           string
	Type         string
	TokenAddress string
	NetworkID    string
	ThesisID     string
	CreatedAt    time.Time
	SeenAt       time.Time
	Raw          json.RawMessage
}

func (d *DB) SaveFeedEvents(ctx context.Context, events []FeedEvent) error {
	if len(events) == 0 {
		return nil
	}
	b := &pgx.Batch{}
	for _, e := range events {
		b.Queue(`
INSERT INTO feed_events (event_id, type, token_address, network_id, thesis_id, created_at, first_seen_at, raw)
VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), NULLIF($5, ''), $6, $7, $8)
ON CONFLICT (event_id) DO NOTHING`,
			e.ID, e.Type, e.TokenAddress, e.NetworkID, e.ThesisID, nullTime(e.CreatedAt), e.SeenAt, cleanJSON(e.Raw))
	}
	return d.sendBatch(ctx, b)
}

// SaveDetails stores a /proxy/tokenDetails response.
func (d *DB) SaveDetails(ctx context.Context, mint string, at time.Time, data json.RawMessage) error {
	data = cleanJSON(data)
	return pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
INSERT INTO token_snapshots (token_address, observed_at, source, data) VALUES ($1, $2, 'tokenDetails', $3)`,
			mint, at, data); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE tokens SET details = $2, details_at = $3, updated_at = $3 WHERE token_address = $1`,
			mint, data, at)
		return err
	})
}

// SaveSnapshots stores per-token pieces of a batch response.
func (d *DB) SaveSnapshots(ctx context.Context, source string, at time.Time, perToken map[string]json.RawMessage) error {
	if len(perToken) == 0 {
		return nil
	}
	b := &pgx.Batch{}
	for mint, data := range perToken {
		b.Queue(`INSERT INTO token_snapshots (token_address, observed_at, source, data) VALUES ($1, $2, $3, $4)`,
			mint, at, source, cleanJSON(data))
	}
	return d.sendBatch(ctx, b)
}

func (d *DB) SaveAPIResponse(ctx context.Context, endpoint string, at time.Time, request any, response json.RawMessage) error {
	_, err := d.pool.Exec(ctx,
		`INSERT INTO api_responses (observed_at, endpoint, request, response) VALUES ($1, $2, $3, $4)`,
		at, endpoint, jsonOrNil(request), cleanJSON(response))
	return err
}

// SaveTrending stores a trending message and the tokens it lists, in order.
func (d *DB) SaveTrending(ctx context.Context, at time.Time, raw json.RawMessage, mints []string) error {
	return pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		var id int64
		if err := tx.QueryRow(ctx,
			`INSERT INTO trending_snapshots (received_at, token_count, raw) VALUES ($1, $2, $3) RETURNING id`,
			at, len(mints), cleanJSON(raw)).Scan(&id); err != nil {
			return err
		}
		rows := make([][]any, len(mints))
		for i, m := range mints {
			rows[i] = []any{id, m, i + 1, at}
		}
		_, err := tx.CopyFrom(ctx, pgx.Identifier{"trending_sightings"},
			[]string{"snapshot_id", "token_address", "position", "observed_at"}, pgx.CopyFromRows(rows))
		return err
	})
}

func (d *DB) SaveWSMessage(ctx context.Context, at time.Time, typ string, raw json.RawMessage) error {
	_, err := d.pool.Exec(ctx, `INSERT INTO ws_messages (received_at, type, raw) VALUES ($1, NULLIF($2, ''), $3)`,
		at, typ, cleanJSON(raw))
	return err
}

// APICall is a row of the HTTP request journal.
type APICall struct {
	At       time.Time
	Method   string
	Path     string
	Query    string
	Status   int
	Duration time.Duration
	Bytes    int
	Error    string
}

func (d *DB) SaveAPICalls(ctx context.Context, runID int64, calls []APICall) error {
	if len(calls) == 0 {
		return nil
	}
	rows := make([][]any, len(calls))
	for i, c := range calls {
		var status *int
		if c.Status != 0 {
			status = &c.Status
		}
		rows[i] = []any{nullID(runID), c.At, c.Method, c.Path, c.Query, status,
			int(c.Duration.Milliseconds()), c.Bytes, nullString(c.Error)}
	}
	_, err := d.pool.CopyFrom(ctx, pgx.Identifier{"api_calls"},
		[]string{"run_id", "at", "method", "path", "query", "status", "duration_ms", "bytes", "error"},
		pgx.CopyFromRows(rows))
	return err
}

// KnownToken is the state the recorder restores after a restart.
type KnownToken struct {
	Address         string
	FirstSeenAt     time.Time
	LastActivityAt  time.Time
	FirstThesisAt   time.Time
	HistoryComplete bool
	Count           int
	CountCheckedAt  time.Time
	DetailsAt       time.Time
}

func (d *DB) LoadTokens(ctx context.Context, activeSince time.Time) ([]KnownToken, error) {
	rows, err := d.pool.Query(ctx, `
SELECT token_address, first_seen_at, last_activity_at, first_thesis_at, history_complete,
       COALESCE(thesis_count, 0), count_checked_at, details_at
FROM tokens WHERE last_activity_at >= $1`, activeSince)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (KnownToken, error) {
		var k KnownToken
		var first, checked, details *time.Time
		err := r.Scan(&k.Address, &k.FirstSeenAt, &k.LastActivityAt, &first, &k.HistoryComplete,
			&k.Count, &checked, &details)
		k.FirstThesisAt, k.CountCheckedAt, k.DetailsAt = deref(first), deref(checked), deref(details)
		return k, err
	})
}

// Counts returns row counts of the main tables, for progress logs.
func (d *DB) Counts(ctx context.Context) (map[string]int64, error) {
	tables := []string{"tokens", "theses", "thesis_observations", "thesis_counts", "token_snapshots", "trending_snapshots"}
	out := make(map[string]int64, len(tables))
	for _, t := range tables {
		var n int64
		if err := d.pool.QueryRow(ctx, "SELECT count(*) FROM "+t).Scan(&n); err != nil {
			return nil, err
		}
		out[t] = n
	}
	return out, nil
}

func (d *DB) sendBatch(ctx context.Context, b *pgx.Batch) error {
	return pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		return tx.SendBatch(ctx, b).Close()
	})
}

// Postgres rejects NUL in text and \u0000 in jsonb.
func clean(s string) string { return strings.ReplaceAll(s, "\x00", "") }

func cleanJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	if !bytes.Contains(raw, []byte(`\u0000`)) {
		return raw
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	out, err := json.Marshal(stripNUL(v))
	if err != nil {
		return raw
	}
	return out
}

func stripNUL(v any) any {
	switch x := v.(type) {
	case string:
		return clean(x)
	case []any:
		for i := range x {
			x[i] = stripNUL(x[i])
		}
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[clean(k)] = stripNUL(val)
		}
		return out
	}
	return v
}

func jsonOrNil(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case json.RawMessage:
		if len(x) == 0 || string(x) == "null" {
			return nil
		}
		return cleanJSON(x)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		return json.RawMessage(b)
	}
}

func nullID(id int64) *int64 {
	if id == 0 {
		return nil
	}
	return &id
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
