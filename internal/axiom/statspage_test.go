package axiom

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"fomobot/internal/domain"
	"fomobot/internal/session"
)

func TestStatsPageDropsPairCookiesAndWaitsForItsWindow(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	page, err := NewStatsPage(Config{
		Chrome:  session.ChromeOptions{ProfileDir: t.TempDir()},
		Cookies: map[string]string{"auth-access-token": "pair-account"},
	}, log)
	if err != nil {
		t.Fatal(err)
	}
	if page.cfg.Cookies != nil {
		t.Fatal("stats window kept the pair account cookies")
	}
	_, failed, err := page.Volumes(context.Background(), []Ask{{Mint: "m", Pair: "pair"}})
	if err == nil || len(failed) != 1 || failed[0] != "m" {
		t.Fatalf("closed window: failed=%v err=%v", failed, err)
	}
	if _, err := NewStatsPage(Config{}, log); err == nil {
		t.Fatal("empty profile must fail")
	}
}

func TestPaceBacksOffOn429AndEasesAfterQuietAnswers(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	page, err := NewStatsPage(Config{Chrome: session.ChromeOptions{ProfileDir: t.TempDir()}}, log)
	if err != nil {
		t.Fatal(err)
	}
	if page.gap != statsGapStart {
		t.Fatalf("gap %s", page.gap)
	}
	page.observeRows([]pageRow{{Status: 429}})
	if page.gap != time.Second {
		t.Fatalf("after 429 gap %s", page.gap)
	}
	page.observeRows([]pageRow{{Status: 404}})
	if page.gap != 2*time.Second {
		t.Fatalf("after 404 gap %s", page.gap)
	}
	for range statsEaseEvery {
		page.observeRows([]pageRow{{Status: 200}})
	}
	if page.gap != 1600*time.Millisecond {
		t.Fatalf("after quiet gap %s", page.gap)
	}
	page.gap = statsGapCeil
	page.observeRows([]pageRow{{Status: 429}})
	if page.gap != statsGapCeil {
		t.Fatalf("ceiling gap %s", page.gap)
	}
}

func TestClosedPageDoesNotPace(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	page, err := NewStatsPage(Config{Chrome: session.ChromeOptions{ProfileDir: t.TempDir()}}, log)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, failed, err := page.Volumes(context.Background(), []Ask{{Mint: "a", Pair: "p"}, {Mint: "b", Pair: "q"}})
	if err == nil || len(failed) != 2 || time.Since(start) > 150*time.Millisecond {
		t.Fatalf("failed=%v err=%v after %s", failed, err, time.Since(start))
	}
}

func TestWaitTurnSpacesStarts(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	page, err := NewStatsPage(Config{Chrome: session.ChromeOptions{ProfileDir: t.TempDir()}}, log)
	if err != nil {
		t.Fatal(err)
	}
	page.gap = 60 * time.Millisecond
	start := time.Now()
	if err := page.waitTurn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := page.waitTurn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Fatalf("second read started after %s", time.Since(start))
	}
}

func TestVolumesFromRows(t *testing.T) {
	young := `{"ath":{"marketCap":24584,"timestamp":"2026-10-07T11:23:00Z"},"windows":[[522,352,30100.5,23100.5,40.1],[522,352,30100.5,23100.5,40.1],[522,352,30100.5,23100.5,40.1],[522,352,30100.5,23100.5,40.1]]}`
	quiet := `{"ath":{"marketCap":10418},"windows":[[0,0,0,0,0],[0,0,0,0,0],[6,9,1252.62225,409.51026921557,-10.8],[6,9,1252.62225,409.51026921557,-10.8]]}`
	vols, failed, err := volumesFromRows([]pageRow{
		{Mint: "young", Status: 200, Body: young},
		{Mint: "quiet", Status: 200, Body: quiet},
		{Mint: "none", Status: 200, Body: `{"ath":{"marketCap":60051},"windows":null}`},
		{Mint: "short", Status: 200, Body: `{"windows":[[1,2,3,4,5]]}`},
		{Mint: "down", Status: 401, Body: `{"error":"Session invalid, please login again"}`},
		{Mint: "bad", Error: "timeout"},
		{Mint: "junk", Status: 200, Body: "<html>"},
	})
	if got := vols["young"]; got != (domain.Volume{USD5m: 53201, USD1h: 53201, Trades5m: 874, Trades1h: 874}) {
		t.Fatalf("young = %+v", got)
	}
	if got := vols["quiet"]; got != (domain.Volume{}) {
		t.Fatalf("quiet = %+v", got)
	}
	if got, ok := vols["none"]; !ok || got != (domain.Volume{}) {
		t.Fatalf("windows null = %+v ok=%v", got, ok)
	}
	if got := vols["short"]; got != (domain.Volume{USD5m: 7, Trades5m: 3}) {
		t.Fatalf("one window = %+v", got)
	}
	if len(failed) != 3 || err == nil {
		t.Fatalf("failed=%v err=%v", failed, err)
	}
}
