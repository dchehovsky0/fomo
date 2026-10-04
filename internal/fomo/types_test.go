package fomo

import (
	"os"
	"testing"
	"time"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// testdata/*_response.json are real responses captured from fomo.family.

func TestParseRealFeed(t *testing.T) {
	events, errs, err := ParseFeed(readFixture(t, "feed_thesis_response.json"))
	if err != nil || len(errs) != 0 {
		t.Fatal(err, errs)
	}
	if len(events) != 8 {
		t.Fatalf("got %d events, want 8", len(events))
	}
	solana := 0
	for _, e := range events {
		if !e.IsThesis() {
			t.Errorf("event %s has type %q", e.ID, e.Type)
		}
		if e.NetworkID == SolanaNetworkID {
			solana++
		}
	}
	if solana != 6 {
		t.Errorf("solana events = %d, want 6", solana)
	}

	e := events[0]
	if e.ID != "a1fab617-7e1a-4a88-8f8f-51b3f5852bdf" || len(e.Raw) == 0 {
		t.Errorf("id = %q, raw kept = %v", e.ID, len(e.Raw) > 0)
	}
	th := e.Thesis()
	if th.Ticker != "e/acc" || th.Handle != "Binnje941" || th.TokenAddress != "CbcyNo7m1amFWqEQm2m4PLv1UNvpcL3C1Ujm6AkzpKoU" {
		t.Errorf("thesis = %+v", th)
	}
	if th.PositionUSD != 35170.52 || th.MarketCap != 17560600 || th.MarketCapAtCreation != 14839431.01001936 {
		t.Errorf("position=%v mc=%v mcAtCreation=%v", th.PositionUSD, th.MarketCap, th.MarketCapAtCreation)
	}
	if th.IsDev || th.Verified || th.UserID != "8fc6597c-832b-5081-b6dc-d9be60699491" {
		t.Errorf("isDev=%v verified=%v user=%q", th.IsDev, th.Verified, th.UserID)
	}
	if want := time.Date(2026, 9, 26, 13, 41, 10, 0, time.UTC); !th.CreatedAt.Equal(want) {
		t.Errorf("createdAt = %v, want %v", th.CreatedAt, want)
	}
	if e.Body.CommentID != "t2-77713238c073543bedc99b03c98b9f75d7615286dcc49aa0967c8aa79393f185-c511f120" {
		t.Errorf("commentId = %q", e.Body.CommentID)
	}
}

func TestParseRealTokenThesis(t *testing.T) {
	page, errs, err := ParseTokenThesis(readFixture(t, "token_thesis_response.json"))
	if err != nil || len(errs) != 0 {
		t.Fatal(err, errs)
	}
	if page.Count != 663 || !page.HasNextPage || len(page.Items) != 25 {
		t.Fatalf("count=%d hasNext=%v items=%d", page.Count, page.HasNextPage, len(page.Items))
	}
	if page.LastID != "t2-7ef9cb938477e2f1bd4eb87e74890da240347dcf7258bbdcf5ecbee9afbad7ae-5ee3e2fa" {
		t.Errorf("LastID = %q", page.LastID)
	}

	first := page.Items[0]
	if first.Handle != "PureArmedPony" || first.Ticker != "DEBT" || first.TokenAddress != "FYM6PhwAS3QwTBwvgLj8JYeMgpNfKsWtW3hGxPM9eFkt" {
		t.Errorf("first = %+v", first)
	}
	if first.PositionUSD != 1352.45 || first.PositionClosed || first.MarketCapAtCreation != 532232.9145141674 {
		t.Errorf("position=%v closed=%v mcAtCreation=%v", first.PositionUSD, first.PositionClosed, first.MarketCapAtCreation)
	}
	if first.Comment != "Send it to trillions, make JPOW proud." || first.NetworkID != SolanaNetworkID {
		t.Errorf("comment=%q network=%q", first.Comment, first.NetworkID)
	}

	var closed, big int
	users := map[string]bool{}
	for i, it := range page.Items {
		if it.PositionClosed {
			closed++
		}
		if it.PositionUSD >= 500 {
			big++
		}
		users[it.UserID] = true
		if i > 0 && it.CreatedAt.After(page.Items[i-1].CreatedAt) {
			t.Errorf("items not newest-first at %d", i)
		}
	}
	if closed != 7 || big != 18 || len(users) != 14 {
		t.Errorf("closed=%d position>=500=%d authors=%d, want 7, 18, 14", closed, big, len(users))
	}
}

func TestParseFeedSkipsBrokenEvent(t *testing.T) {
	body := []byte(`{"success":true,"responseObject":{"feed":[
		{"id":"ok","type":"thesis_created","tokenAddress":"A"},
		"not an object",
		{"id":"bad","type":"thesis_created","tokenAddress":["x"]},
		{"id":"pin","type":"manual","body":"weird"},
		{"id":2,"type":"thesis_created","userId":42,"tokenAddress":"B","createdAt":1790000000000,
		 "body":{"isDev":"true","positionNotionalUsd":"150.25","marketCap":null}}
	]}}`)
	events, errs, err := ParseFeed(body)
	if err != nil {
		t.Fatal(err)
	}
	// The broken manual post is dropped silently; the broken thesis and the
	// non-object are reported.
	if len(events) != 2 || len(errs) != 2 {
		t.Fatalf("events=%d errs=%v, want 2 events and 2 errors", len(events), errs)
	}
	th := events[1].Thesis()
	if th.ID != "2" || th.UserID != "42" || th.CreatedAt.UnixMilli() != 1790000000000 {
		t.Errorf("lenient ids/time: %+v", th)
	}
	if !th.IsDev || th.PositionUSD != 150.25 || th.MarketCap != 0 {
		t.Errorf("lenient body: %+v", th)
	}
}

func TestParseTokenThesisLenient(t *testing.T) {
	body := []byte(`{"success":true,"responseObject":{"items":[
		{"type":"thesis","id":"a","userId":"u1","userHandle":"h1","comment":"plain text","authorTrade":null},
		{"type":"reply","id":"r"},
		{"type":"thesis","id":"b","userId":"u2","comment":{"comment":"obj"},"authorTrade":{"usdValue":"12.5","closedAt":"2026-09-26T13:08:30.448Z"}}
	],"hasNextPage":false,"count":"2"}}`)
	page, errs, err := ParseTokenThesis(body)
	if err != nil || len(errs) != 0 {
		t.Fatal(err, errs)
	}
	if page.Count != 2 || len(page.Items) != 2 || page.LastID != "b" {
		t.Fatalf("page = %+v", page)
	}
	if page.Items[0].Comment != "plain text" || page.Items[0].PositionUSD != 0 {
		t.Errorf("string comment / null trade: %+v", page.Items[0])
	}
	if b := page.Items[1]; b.Comment != "obj" || b.PositionUSD != 12.5 || !b.PositionClosed || b.Handle != "u2" {
		t.Errorf("item b = %+v", b)
	}
}

func TestObservedUsesWhicheverIsLarger(t *testing.T) {
	three := []Thesis{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	if got := (&TokenThesisPage{Count: 2, Items: three}).Observed(); got != 3 {
		t.Fatalf("lagging count: %d", got)
	}
	if got := (&TokenThesisPage{Count: 0, Items: three}).Observed(); got != 3 {
		t.Fatalf("zero count: %d", got)
	}
	if got := (&TokenThesisPage{Count: 10, Items: three[:2]}).Observed(); got != 10 {
		t.Fatalf("count ahead of this page: %d", got)
	}
	if (*TokenThesisPage)(nil).Observed() != 0 {
		t.Fatal("nil page")
	}
}

func TestEnvelopeError(t *testing.T) {
	_, _, err := ParseFeed([]byte(`{"success":false,"message":"boom","statusCode":500,"responseObject":null}`))
	if err == nil {
		t.Fatal("expected error for success=false")
	}
}
