package recorder

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"fomobot/internal/db"
	"fomobot/internal/fomo"
)

// Integration tests need a disposable database, e.g.
//   docker run -d -e POSTGRES_PASSWORD=test -e POSTGRES_DB=fomo_test -p 55432:5432 postgres:17-alpine
//   FOMO_TEST_DATABASE_URL=postgres://postgres:test@localhost:55432/fomo_test
// The public schema of that database is dropped before each test.

func testDB(t *testing.T) (*db.DB, *pgx.Conn) {
	t.Helper()
	url := os.Getenv("FOMO_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("FOMO_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close(); conn.Close(context.Background()) })
	return d, conn
}

func scalar[T any](t *testing.T, conn *pgx.Conn, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := conn.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return v
}

type fakeAPI struct {
	mu    sync.Mutex
	feed  []byte
	pages map[string][][]byte
	calls []string
}

func (f *fakeAPI) FeedWithStatus(context.Context, int) ([]fomo.Event, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	events, _, err := fomo.ParseFeed(f.feed)
	return events, false, err
}

func (f *fakeAPI) TokenThesis(_ context.Context, mint, lastID string) (*fomo.TokenThesisPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "thesis:"+mint+":"+lastID)
	prev := ""
	for _, body := range f.pages[mint] {
		p, _, err := fomo.ParseTokenThesis(body)
		if err != nil {
			return nil, err
		}
		if prev == lastID {
			return p, nil
		}
		prev = p.LastID
	}
	if lastID != "" {
		return nil, fmt.Errorf("no page after %s", lastID)
	}
	return &fomo.TokenThesisPage{}, nil
}

func (f *fakeAPI) TokenDetails(_ context.Context, mint string) (json.RawMessage, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "details:"+mint)
	f.mu.Unlock()
	return json.RawMessage(`{"token":{"address":"` + mint + `","createdAt":1758900000}}`), nil
}

func (f *fakeAPI) FilterTokens(_ context.Context, mints []string) (json.RawMessage, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fmt.Sprintf("filter:%d", len(mints)))
	f.mu.Unlock()
	var parts []string
	for _, m := range mints {
		parts = append(parts, `{"token":{"address":"`+m+`"},"marketCap":123}`)
	}
	return json.RawMessage(`{"results":[` + strings.Join(parts, ",") + `]}`), nil
}

func feedBody(events ...string) []byte {
	return []byte(`{"success":true,"responseObject":{"feed":[` + strings.Join(events, ",") + `]}}`)
}

func feedEvent(eventID, thesisID, mint string, created time.Time, position float64) string {
	return fmt.Sprintf(`{"id":%q,"type":"thesis_created","userId":"u-%s","tradeId":"tr-%s","tokenAddress":%q,
		"networkId":1399811149,"createdAt":%q,"verified":true,"likes":1,"views":7,"numReplies":0,
		"body":{"ticker":"NEW","comment":"gm","commentId":%q,"userHandle":"h-%s","positionNotionalUsd":%v,
		"marketCap":50000,"price":0.00005,"marketCapAtCreation":40000,"isDev":false,
		"tokenImageUrl":"https://img/new.png"},
		"tradeComment":{"id":%q,"olderThesis":0,"newerThesis":0}}`,
		eventID, eventID, eventID, mint, created.UTC().Format(time.RFC3339Nano), thesisID, eventID, position, thesisID)
}

func thesisPage(count int, hasNext bool, items ...string) []byte {
	return []byte(fmt.Sprintf(`{"success":true,"responseObject":{"items":[%s],"hasNextPage":%v,"count":%d}}`,
		strings.Join(items, ","), hasNext, count))
}

func thesisItem(id, mint string, created time.Time, usd float64) string {
	return fmt.Sprintf(`{"type":"thesis","id":%q,"tradeId":"tr-%s","createdAt":%q,"userId":"u-%s",
		"userHandle":"h-%s","verified":false,"isDev":false,
		"comment":{"id":%q,"comment":"gm","marketCapAtCreation":40000,"priceUsdAtCreation":0.00004,"olderThesis":0},
		"authorTrade":{"usdValue":%v,"humanTokenAmount":10,"closedAt":null},
		"tokenAddress":%q,"networkId":1399811149,"ticker":"NEW","equity":0,"threshold":%v}`,
		id, id, created.UTC().Format(time.RFC3339Nano), id, id, id, usd, mint, usd)
}

func newTestRecorder(t *testing.T, d *db.DB, api API) *Recorder {
	runID, err := d.StartRun(context.Background(), time.Now(), map[string]any{"test": true})
	if err != nil {
		t.Fatal(err)
	}
	return New(Config{
		NetworkID: sol, FeedLimit: 50, FeedInterval: time.Second, HistoryMaxPages: 5,
		RefreshMinInterval: 10 * time.Second, NewTokenRefresh: 30 * time.Second, NewTokenTrackFor: 2 * time.Hour,
		AllTokensRefresh: 15 * time.Minute, TrackFor: 6 * time.Hour, SnapshotInterval: time.Minute,
		SnapshotBatch: 2, DetailsInterval: 30 * time.Minute, TrendingPages: 1,
	}, api, d, runID, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// drain runs queued jobs that are due now, like the worker does.
func drain(t *testing.T, r *Recorder) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for range 200 {
		pctx, pcancel := context.WithTimeout(ctx, 50*time.Millisecond)
		j, ok := r.queue.pop(pctx, r.now)
		pcancel()
		if !ok {
			return
		}
		r.runJob(ctx, j)
	}
	t.Fatal("queue did not drain")
}

func TestRecorderEndToEnd(t *testing.T) {
	d, conn := testDB(t)
	ctx := context.Background()

	fixture, err := os.ReadFile("../fomo/testdata/feed_thesis_response.json")
	if err != nil {
		t.Fatal(err)
	}
	debt, err := os.ReadFile("../fomo/testdata/token_thesis_response.json")
	if err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{feed: fixture, pages: map[string][][]byte{mintA: {debt}}}
	r := newTestRecorder(t, d, api)

	events, _, _ := fomo.ParseFeed(fixture)
	solMints := map[string]bool{}
	solTheses := 0
	for _, e := range events {
		if e.IsThesis() && e.NetworkID.String() == sol {
			solTheses++
			solMints[e.TokenAddress] = true
		}
	}

	// 1. Startup feed: everything is recorded, tokens are marked as pre-existing.
	r.handleFeed(ctx, events, true)
	if n := scalar[int64](t, conn, `SELECT count(*) FROM feed_events`); n != int64(len(events)) {
		t.Errorf("feed_events = %d, want %d", n, len(events))
	}
	if n := scalar[int64](t, conn, `SELECT count(*) FROM theses WHERE raw_feed IS NOT NULL`); n != int64(solTheses) {
		t.Errorf("theses from feed = %d, want %d", n, solTheses)
	}
	if n := scalar[int64](t, conn, `SELECT count(*) FROM tokens WHERE first_seen_source = 'startup_feed'`); n != int64(len(solMints)) {
		t.Errorf("startup tokens = %d, want %d", n, len(solMints))
	}
	if n := scalar[int64](t, conn, `SELECT count(*) FROM tokens WHERE first_feed_thesis_at IS NOT NULL`); n != 0 {
		t.Errorf("startup theses must not count as live, got %d tokens", n)
	}
	if n := scalar[int64](t, conn, `SELECT count(*) FROM users`); n == 0 {
		t.Error("authors not recorded")
	}
	obs := scalar[int64](t, conn, `SELECT count(*) FROM thesis_observations`)

	// The same feed again changes nothing.
	r.handleFeed(ctx, events, false)
	if n := scalar[int64](t, conn, `SELECT count(*) FROM thesis_observations`); n != obs {
		t.Errorf("unchanged feed added observations: %d -> %d", obs, n)
	}
	drain(t, r)

	// 2. A token whose first thesis appears while running.
	const mintNew = "So11111111111111111111111111111111111111112"
	now := time.Now()
	api.mu.Lock()
	api.feed = feedBody(feedEvent("ev-new-1", "t2-new-1", mintNew, now, 1234.5))
	api.pages[mintNew] = [][]byte{thesisPage(2, false,
		thesisItem("t2-new-1", mintNew, now, 1300),
		thesisItem("t2-new-0", mintNew, now.Add(-20*time.Second), 800))}
	api.mu.Unlock()
	events, _, _ = api.FeedWithStatus(ctx, 50)
	r.handleFeed(ctx, events, false)
	drain(t, r)

	if src := scalar[string](t, conn, `SELECT first_seen_source FROM tokens WHERE token_address = $1`, mintNew); src != "feed" {
		t.Errorf("source = %q", src)
	}
	if n := scalar[int64](t, conn, `SELECT count(*) FROM v_new_tokens WHERE token_address = $1 AND thesis_count = 2`, mintNew); n != 1 {
		t.Error("token born while recording is not in v_new_tokens")
	}
	merged := scalar[bool](t, conn, `SELECT raw_feed IS NOT NULL AND raw_token IS NOT NULL AND feed_event_id = 'ev-new-1'
		AND seen_in_feed_at IS NOT NULL AND position_usd = 1300 AND token_market_cap = 50000 AND threshold = 1300
		FROM theses WHERE thesis_id = 't2-new-1'`)
	if !merged {
		t.Error("feed and token endpoint observations were not merged into one thesis row")
	}
	if n := scalar[int64](t, conn, `SELECT count(*) FROM thesis_observations WHERE thesis_id = 't2-new-1'`); n != 2 {
		t.Errorf("observations of t2-new-1 = %d, want 2 (feed + token endpoint)", n)
	}
	if n := scalar[int64](t, conn, `SELECT n FROM v_thesis_timeline WHERE thesis_id = 't2-new-1'`); n != 2 {
		t.Errorf("timeline position = %d, want 2", n)
	}
	if n := scalar[int64](t, conn, `SELECT count(*) FROM v_new_tokens WHERE first_seen_source = 'startup_feed'`); n != 0 {
		t.Errorf("pre-existing tokens in v_new_tokens: %d", n)
	}

	// 3. A new thesis on a known token schedules an urgent refresh.
	api.mu.Lock()
	api.feed = feedBody(feedEvent("ev-new-2", "t2-new-2", mintNew, now.Add(time.Second), 10))
	api.mu.Unlock()
	events, _, _ = api.FeedWithStatus(ctx, 50)
	r.handleFeed(ctx, events, false)
	r.queue.mu.Lock()
	j, ok := r.queue.jobs[jobKey{jobRefresh, mintNew}]
	r.queue.mu.Unlock()
	if !ok || j.prio != prioUrgent {
		t.Errorf("refresh job = %+v, queued %v", j, ok)
	}

	// 4. Trending: sightings stored, unseen tokens looked at with one page.
	r.OnTrending(ctx, time.Now(), json.RawMessage(`{"type":"update","data":[]}`), []string{mintB, mintNew})
	if n := scalar[int64](t, conn, `SELECT count(*) FROM trending_sightings`); n != 2 {
		t.Errorf("trending sightings = %d", n)
	}
	if !solMints[mintB] {
		if src := scalar[string](t, conn, `SELECT first_seen_source FROM tokens WHERE token_address = $1`, mintB); src != "trending" {
			t.Errorf("trending token source = %q", src)
		}
	}

	// 5. Batch snapshots and details.
	r.snapshotOnce(ctx)
	tracked, _ := r.Tracked()
	if n := scalar[int64](t, conn, `SELECT count(*) FROM token_snapshots WHERE source = 'filterTokens'`); n != int64(tracked) {
		t.Errorf("filterTokens snapshots = %d, want %d", n, tracked)
	}
	if n := scalar[int64](t, conn, `SELECT count(*) FROM api_responses`); n != 1 {
		t.Errorf("api_responses = %d, want only the reference one", n)
	}
	if err := r.fetchDetails(ctx, mintNew); err != nil {
		t.Fatal(err)
	}
	if ok := scalar[bool](t, conn, `SELECT details IS NOT NULL FROM tokens WHERE token_address = $1`, mintNew); !ok {
		t.Error("details not stored")
	}

	// 6. DEBT has 663 theses; only one page is available, so its history stays incomplete.
	api.mu.Lock()
	api.feed = feedBody(feedEvent("ev-debt", "t2-debt-live", mintA, time.Now(), 50))
	api.mu.Unlock()
	events, _, _ = api.FeedWithStatus(ctx, 50)
	r.handleFeed(ctx, events, false)
	drain(t, r)
	if ok := scalar[bool](t, conn, `SELECT NOT history_complete AND thesis_count = 663 FROM tokens WHERE token_address = $1`, mintA); !ok {
		t.Error("partial history marked complete")
	}
	if n := scalar[int64](t, conn, `SELECT count(*) FROM theses WHERE token_address = $1 AND raw_token IS NOT NULL`, mintA); n != 25 {
		t.Errorf("DEBT theses from token endpoint = %d, want 25", n)
	}
	if n := scalar[int64](t, conn, `SELECT count(*) FROM v_new_tokens WHERE token_address = $1`, mintA); n != 0 {
		t.Error("an old token with incomplete history is in v_new_tokens")
	}
	tracked, _ = r.Tracked()

	// 7. A restart restores the tracked tokens.
	r2 := newTestRecorder(t, d, api)
	if err := r2.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	if all, _ := r2.Tracked(); all != tracked {
		t.Errorf("restored %d tokens, want %d", all, tracked)
	}
}

func TestCleanJSONRejectsNothing(t *testing.T) {
	d, conn := testDB(t)
	ctx := context.Background()
	raw := json.RawMessage(`{"comment":"bad\u0000byte","x":"\\u0000 literal"}`)
	if err := d.SaveWSMessage(ctx, time.Now(), "t", raw); err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, conn, `SELECT raw->>'x' FROM ws_messages`); got != `\u0000 literal` {
		t.Errorf("escaped text changed: %q", got)
	}
}
