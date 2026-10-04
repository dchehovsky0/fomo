package recorder

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type staticTokens struct{}

func (staticTokens) Token(context.Context) (string, error) { return "jwt-123", nil }
func (staticTokens) Invalidate()                           {}

func TestTrendingHandshakeAndSampling(t *testing.T) {
	d, conn := testDB(t)
	got := make(chan map[string]string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "https://fomo.family" {
			http.Error(w, "bad origin", http.StatusForbidden)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		read := func() map[string]string {
			_, b, err := c.Read(ctx)
			if err != nil {
				return nil
			}
			var m map[string]string
			_ = json.Unmarshal(b, &m)
			got <- m
			return m
		}
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"challenge","nonce":"n1"}`))
		if m := read(); m["jwt"] != "jwt-123" {
			return
		}
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"challengeAccepted"}`))
		read()
		for range 3 {
			_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"topicUpdate","topicType":"trending_tokens","data":[
				{"address":"`+mintA+`","networkId":1399811149},{"address":"0x1234567890123456789012345678901234567890","networkId":8453}]}`))
		}
		<-ctx.Done()
	}))
	defer srv.Close()

	api := &fakeAPI{pages: map[string][][]byte{}}
	r := newTestRecorder(t, d, api)
	tr := NewTrending(TrendingConfig{
		URL: "ws" + strings.TrimPrefix(srv.URL, "http"), TopicID: "1,1399811149", Origin: "https://fomo.family",
		NetworkID: sol, Sample: 300 * time.Millisecond,
	}, staticTokens{}, r, d, r.log)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go tr.Run(ctx)

	resp := <-got
	if resp["type"] != "challengeResponse" {
		t.Fatalf("first client message = %v", resp)
	}
	sub := <-got
	if sub["type"] != "subscribe" || sub["topicType"] != "trending_tokens" || sub["topicId"] != "1,1399811149" {
		t.Fatalf("subscribe = %v", sub)
	}
	for ctx.Err() == nil {
		var n int64
		_ = conn.QueryRow(ctx, `SELECT count(*) FROM trending_sightings WHERE token_address = $1`, mintA).Scan(&n)
		if n > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if n := scalar[int64](t, conn, `SELECT count(*) FROM trending_snapshots`); n != 1 {
		t.Errorf("three messages within one sample must give one snapshot, got %d", n)
	}
	if src := scalar[string](t, conn, `SELECT first_seen_source FROM tokens WHERE token_address = $1`, mintA); src != "trending" {
		t.Errorf("source = %q", src)
	}
	if n := scalar[int64](t, conn, `SELECT count(*) FROM ws_messages WHERE type IN ('challenge', 'challengeAccepted')`); n != 2 {
		t.Errorf("control messages stored = %d", n)
	}
}
