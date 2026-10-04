package recorder

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"
)

const (
	mintA = "FYM6PhwAS3QwTBwvgLj8JYeMgpNfKsWtW3hGxPM9eFkt"
	mintB = "CbcyNo7m1amFWqEQm2m4PLv1UNvpcL3C1Ujm6AkzpKoU"
	sol   = "1399811149"
)

func TestSolanaMints(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"list with network ids", `{"type":"update","data":[
			{"address":"` + mintA + `","networkId":1399811149,"pair":{"address":"0xabc"}},
			{"address":"0x1234567890123456789012345678901234567890","networkId":8453},
			{"tokenAddress":"` + mintB + `","networkId":"1399811149"}]}`,
			[]string{mintA, mintB}},
		{"token ids with network suffix", `[{"id":"` + mintB + `:1399811149"},{"id":"` + mintA + `:8453"}]`,
			[]string{mintB}},
		{"other network excluded even if base58", `[{"address":"` + mintA + `","networkId":56}]`, nil},
		{"uuid and pair address ignored", `{"id":"8fc6597c-832b-5081-b6dc-d9be60699491","pairAddress":"` + mintA + `"}`, nil},
		{"duplicates removed, order kept", `[{"mint":"` + mintB + `"},{"mint":"` + mintA + `"},{"mint":"` + mintB + `"}]`,
			[]string{mintB, mintA}},
		{"not json", `hello`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := solanaMints([]byte(c.raw), sol); !slices.Equal(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestSplitByToken(t *testing.T) {
	for name, raw := range map[string]string{
		"bare array":     `[{"token":{"address":"` + mintA + `"},"mc":1},{"token":{"address":"` + mintB + `"},"mc":2}]`,
		"wrapped array":  `{"count":2,"results":[{"id":"` + mintA + `:1399811149"},{"id":"` + mintB + `:1399811149"}]}`,
		"nested wrapper": `{"data":{"filterTokens":{"results":[{"a":"` + mintA + `"},{"a":"` + mintB + `"}]}}}`,
		"keyed object":   `{"` + mintA + `:1399811149":{"mc":1},"` + mintB + `:1399811149":{"mc":2}}`,
	} {
		t.Run(name, func(t *testing.T) {
			parts := splitByToken(json.RawMessage(raw), []string{mintA, mintB})
			if len(parts) != 2 || !json.Valid(parts[mintA]) || !json.Valid(parts[mintB]) {
				t.Fatalf("parts = %v", parts)
			}
		})
	}
	if parts := splitByToken(json.RawMessage(`{"error":"x"}`), []string{mintA}); len(parts) != 0 {
		t.Errorf("unexpected parts %v", parts)
	}
}

func TestQueuePriorityAndMerge(t *testing.T) {
	q := newQueue()
	now := time.Unix(1000, 0)
	clock := func() time.Time { return now }
	q.push(job{kind: jobDetails, mint: "a", prio: prioLow, due: now})
	q.push(job{kind: jobRefresh, mint: "b", prio: prioBackfill, due: now})
	q.push(job{kind: jobRefresh, mint: "b", prio: prioUrgent, due: now.Add(time.Minute)}) // merged: urgent, keeps earlier due
	q.push(job{kind: jobRefresh, mint: "c", prio: prioUrgent, due: now.Add(time.Hour)})   // not due
	if q.len() != 3 {
		t.Fatalf("len = %d", q.len())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	j, _ := q.pop(ctx, clock)
	if j.mint != "b" || j.prio != prioUrgent {
		t.Fatalf("first = %+v", j)
	}
	j, _ = q.pop(ctx, clock)
	if j.mint != "a" {
		t.Fatalf("second = %+v", j)
	}
	if _, ok := q.pop(ctx, clock); ok {
		t.Fatal("job c is not due and must not pop")
	}
}
