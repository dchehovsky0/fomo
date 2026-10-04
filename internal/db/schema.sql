-- Applied on every start; every statement is idempotent.

CREATE TABLE IF NOT EXISTS runs (
    id          bigserial PRIMARY KEY,
    started_at  timestamptz NOT NULL,
    stopped_at  timestamptz,
    config      jsonb
);

-- One row per token ever seen on fomo.
CREATE TABLE IF NOT EXISTS tokens (
    token_address        text PRIMARY KEY,
    network_id           text NOT NULL,
    ticker               text,
    image_url            text,
    first_seen_at        timestamptz NOT NULL,
    -- startup_feed: in the feed at service start (existed before);
    -- feed: a thesis appeared while running; trending: fomo trending list.
    first_seen_source    text NOT NULL,
    first_seen_run       bigint REFERENCES runs(id),
    first_feed_thesis_at timestamptz,  -- first thesis seen live in the feed
    first_thesis_at      timestamptz,  -- oldest thesis known (exact when history_complete)
    history_complete     boolean NOT NULL DEFAULT false,
    thesis_count         integer,      -- latest /feed/token/thesis count
    thesis_count_at_discovery integer,
    count_checked_at     timestamptz,
    details              jsonb,        -- latest /proxy/tokenDetails
    details_at           timestamptz,
    last_activity_at     timestamptz NOT NULL,
    updated_at           timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS users (
    user_id       text PRIMARY KEY,
    handle        text,
    display_name  text,
    verified      boolean,
    twitter       jsonb,
    image_url     text,
    first_seen_at timestamptz NOT NULL,
    last_seen_at  timestamptz NOT NULL
);

-- Latest known state of each thesis, merged from /feed and /feed/token/thesis.
CREATE TABLE IF NOT EXISTS theses (
    thesis_id              text PRIMARY KEY,
    token_address          text NOT NULL,
    network_id             text,
    ticker                 text,
    user_id                text,
    user_handle            text,
    display_name           text,
    verified               boolean,
    is_dev                 boolean,
    twitter                jsonb,
    created_at             timestamptz NOT NULL,
    trade_id               text,
    swap_id                text,
    transfer_id            text,
    feed_event_id          text,
    comment                text,
    price_usd_at_creation  double precision,
    market_cap_at_creation double precision,
    fdv_at_creation        double precision,
    first_seen_at          timestamptz NOT NULL,
    first_seen_source      text NOT NULL,   -- feed | token_thesis
    seen_in_feed_at        timestamptz,     -- first seen in the live feed
    -- mutable, latest observation
    position_usd           double precision,
    human_token_amount     double precision,
    realized_pnl_usd       double precision,
    unrealized_pnl_usd     double precision,
    pct_realized_pnl       double precision,
    pct_unrealized_pnl     double precision,
    position_closed_at     timestamptz,
    equity                 double precision,
    threshold              double precision,
    likes                  integer,
    views                  integer,
    num_replies            integer,
    older_thesis           integer,
    newer_thesis           integer,
    token_market_cap       double precision, -- token market cap at the latest feed observation
    token_price            double precision,
    token_fdv              double precision,
    updated_at             timestamptz NOT NULL,
    raw_feed               jsonb,
    raw_token              jsonb
);
CREATE INDEX IF NOT EXISTS theses_token_created ON theses (token_address, created_at);
CREATE INDEX IF NOT EXISTS theses_user ON theses (user_id);

-- Every changed state of a thesis as it was observed (position, PnL, market cap).
CREATE TABLE IF NOT EXISTS thesis_observations (
    id                 bigserial PRIMARY KEY,
    thesis_id          text NOT NULL,
    token_address      text NOT NULL,
    observed_at        timestamptz NOT NULL,
    source             text NOT NULL,
    position_usd       double precision,
    human_token_amount double precision,
    realized_pnl_usd   double precision,
    unrealized_pnl_usd double precision,
    position_closed_at timestamptz,
    token_market_cap   double precision,
    token_price        double precision,
    likes              integer,
    views              integer,
    num_replies        integer,
    raw                jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS thesis_observations_thesis ON thesis_observations (thesis_id, observed_at);
CREATE INDEX IF NOT EXISTS thesis_observations_token ON thesis_observations (token_address, observed_at);

-- Raw /feed events, including non-thesis ones (pinned posts).
CREATE TABLE IF NOT EXISTS feed_events (
    event_id      text PRIMARY KEY,
    type          text,
    token_address text,
    network_id    text,
    thesis_id     text,
    created_at    timestamptz,
    first_seen_at timestamptz NOT NULL,
    raw           jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS feed_events_token ON feed_events (token_address, created_at);

-- Thesis count of a token over time.
CREATE TABLE IF NOT EXISTS thesis_counts (
    id            bigserial PRIMARY KEY,
    token_address text NOT NULL,
    observed_at   timestamptz NOT NULL,
    count         integer NOT NULL,
    has_next_page boolean,
    reason        text
);
CREATE INDEX IF NOT EXISTS thesis_counts_token ON thesis_counts (token_address, observed_at);

-- Token metrics over time: source = filterTokens | tokenDetails.
CREATE TABLE IF NOT EXISTS token_snapshots (
    id            bigserial PRIMARY KEY,
    token_address text NOT NULL,
    observed_at   timestamptz NOT NULL,
    source        text NOT NULL,
    data          jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS token_snapshots_token ON token_snapshots (token_address, observed_at);

-- Whole responses of endpoints whose format is not mapped yet.
CREATE TABLE IF NOT EXISTS api_responses (
    id          bigserial PRIMARY KEY,
    observed_at timestamptz NOT NULL,
    endpoint    text NOT NULL,
    request     jsonb,
    response    jsonb NOT NULL
);

CREATE TABLE IF NOT EXISTS trending_snapshots (
    id          bigserial PRIMARY KEY,
    received_at timestamptz NOT NULL,
    token_count integer NOT NULL,
    raw         jsonb NOT NULL
);

CREATE TABLE IF NOT EXISTS trending_sightings (
    snapshot_id   bigint NOT NULL REFERENCES trending_snapshots(id),
    token_address text NOT NULL,
    position      integer NOT NULL,
    observed_at   timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS trending_sightings_token ON trending_sightings (token_address, observed_at);

-- WebSocket control and non-trending messages.
CREATE TABLE IF NOT EXISTS ws_messages (
    id          bigserial PRIMARY KEY,
    received_at timestamptz NOT NULL,
    type        text,
    raw         jsonb NOT NULL
);

CREATE TABLE IF NOT EXISTS api_calls (
    id          bigserial PRIMARY KEY,
    run_id      bigint,
    at          timestamptz NOT NULL,
    method      text NOT NULL,
    path        text NOT NULL,
    query       text,
    status      integer,
    duration_ms integer,
    bytes       integer,
    error       text
);
CREATE INDEX IF NOT EXISTS api_calls_at ON api_calls (at);

-- Tokens this bot watches after the market-cap gate. tier is the current
-- tier (1–4). tier_history is every tier the token has been in, oldest first:
-- [{"tier":1,"at":"..."},{"tier":2,"at":"..."}].
CREATE TABLE IF NOT EXISTS watch_tokens (
    token_address text PRIMARY KEY,
    symbol        text,
    name          text,
    dex           text,
    pool          text,
    created_at    timestamptz,
    entered_at    timestamptz NOT NULL,
    tier          smallint NOT NULL,
    tier_history  jsonb NOT NULL,
    status        text NOT NULL,
    updated_at    timestamptz NOT NULL
);

-- Tokens whose very first thesis appeared while the recorder was running.
CREATE OR REPLACE VIEW v_new_tokens AS
SELECT t.*
FROM tokens t
JOIN runs r ON r.id = t.first_seen_run
WHERE t.history_complete
  AND t.first_thesis_at >= r.started_at - interval '2 minutes';

-- Theses numbered per token in time order: n = 1 is the token's first thesis.
CREATE OR REPLACE VIEW v_thesis_timeline AS
SELECT th.token_address,
       th.ticker,
       row_number() OVER w AS n,
       th.created_at,
       th.created_at - first_value(th.created_at) OVER w AS since_first,
       th.thesis_id,
       th.user_handle,
       th.verified,
       th.is_dev,
       th.position_usd,
       th.market_cap_at_creation,
       th.token_market_cap,
       t.history_complete
FROM theses th
JOIN tokens t USING (token_address)
WINDOW w AS (PARTITION BY th.token_address ORDER BY th.created_at, th.thesis_id);
