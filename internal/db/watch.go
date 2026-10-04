package db

import (
	"context"
	"time"

	"fomobot/internal/domain"
)

// EnterTier inserts a watched token as tier 1. The history field starts with
// that single step. When the mint is already stored, the existing tier, entry
// time and status are returned and the history is left unchanged.
func (d *DB) EnterTier(ctx context.Context, tok domain.Token, enteredAt time.Time) (tier int, entered time.Time, status string, inserted bool, err error) {
	err = d.pool.QueryRow(ctx, `
WITH ins AS (
    INSERT INTO watch_tokens (
        token_address, symbol, name, dex, pool, created_at,
        entered_at, tier, tier_history, status, updated_at)
    VALUES (
        $1, NULLIF($2, ''), NULLIF($3, ''), NULLIF($4, ''), NULLIF($5, ''), $6,
        $7, 1,
        jsonb_build_array(jsonb_build_object('tier', 1, 'at', to_jsonb($7::timestamptz))),
        'watching', $7)
    ON CONFLICT (token_address) DO NOTHING
    RETURNING tier, entered_at, status, TRUE AS inserted
)
SELECT tier, entered_at, status, inserted FROM ins
UNION ALL
SELECT tier, entered_at, status, FALSE FROM watch_tokens
WHERE token_address = $1 AND NOT EXISTS (SELECT 1 FROM ins)`,
		tok.Mint, tok.Symbol, tok.Name, tok.Dex, tok.Pool, nullTime(tok.CreatedAt), enteredAt).
		Scan(&tier, &entered, &status, &inserted)
	return tier, entered, status, inserted, err
}

// AdvanceTier sets the current tier and appends one step to tier_history.
// A repeat of the tier already stored does not add another step.
func (d *DB) AdvanceTier(ctx context.Context, mint string, tier int, at time.Time) error {
	_, err := d.pool.Exec(ctx, `
UPDATE watch_tokens SET
    tier = $2,
    tier_history = tier_history || jsonb_build_array(jsonb_build_object('tier', $2::int, 'at', to_jsonb($3::timestamptz))),
    updated_at = $3
WHERE token_address = $1 AND tier <> $2`,
		mint, tier, at)
	return err
}

// SetWatchStatus records why the token left the tiers, or that it is watched.
// The tier and its history stay as they were.
func (d *DB) SetWatchStatus(ctx context.Context, mint, status string, at time.Time) error {
	_, err := d.pool.Exec(ctx, `
UPDATE watch_tokens SET status = $2, updated_at = $3 WHERE token_address = $1`,
		mint, status, at)
	return err
}
