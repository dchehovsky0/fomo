package notify

import (
	"strings"
	"testing"
	"time"

	"fomobot/internal/domain"
)

var msk = time.FixedZone("MSK", 3*3600)

func sample() Alert {
	now := time.Date(2026, 9, 28, 15, 0, 0, 0, time.UTC)
	created := now.Add(-42 * time.Minute)
	return Alert{
		Kind: KindTheses, Threshold: 3, Token: "MINT123", Symbol: "PRI<ORS", Name: "Priors & Co", Chain: "Solana",
		Dex: "pumpfun", CreatedAt: created, DetectedAt: now,
		Volume: domain.Volume{USD5m: 1500, USD1h: 45_300}, VolumeAt: now.Add(-20 * time.Second),
		CountKnown: true, Count: 3, RateKnown: true, Recent: 3, RateWindow: 10 * time.Minute,
		FirstExact: true,
		First: []Thesis{
			{At: created.Add(12 * time.Minute), Handle: "a", PositionUSD: 27982.8, Text: "long text that must not be in the message"},
			{At: created.Add(31 * time.Minute), Handle: "b", PositionUSD: 640},
			{At: time.Date(2026, 9, 27, 20, 5, 0, 0, time.UTC), Handle: "c", Closed: true},
		},
		FomoURL:  "https://fomo.family/tokens/solana/MINT123",
		AxiomURL: "https://axiom.trade/meme/PAIR?chain=sol&chains=sol,bnb",
	}
}

func TestFormat(t *testing.T) {
	got := Format(sample(), msk)
	for _, want := range []string{
		"🚨 <b>3 тезиса на fomo</b>",
		"<b>$PRI&lt;ORS</b> · Priors &amp; Co",
		"Алерт: 28.09.2026 18:00:00",
		"Chain: Solana · pumpfun · создан 42 мин назад",
		"CA: <code>MINT123</code>",
		"Объём: 1ч $45.3K · 5м $1.5K\n",
		"Тезисов в минуту: 0.3 (3 за 10 мин) · всего 3",
		"<b>Первые 3 тезиса:</b>",
		"1. 17:30:00 — $27,983",
		"2. 17:49:00 — $640",
		"3. 27.09 23:05 — позиция закрыта",
		`<a href="https://fomo.family/tokens/solana/MINT123">fomo</a> | <a href="https://axiom.trade/meme/PAIR?chain=sol&amp;chains=sol,bnb">Axiom</a>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "long text") {
		t.Error("thesis texts belong on the page, not in the message")
	}
}

func TestFormatTrendingAndPartial(t *testing.T) {
	a := sample()
	a.Kind, a.Rank, a.Returned = KindTrending, 5, true
	a.Count, a.Recent, a.RecentCapped = 1_009_102, 500, true
	a.FirstExact = false
	a.CreatedAt, a.Dex = time.Time{}, ""
	got := Format(a, msk)
	for _, want := range []string{
		"🔥 <b>Вернулся в тренды fomo · #5</b>",
		"Chain: Solana\n",
		"Тезисов в минуту: ≥50 (≥500 за 10 мин) · всего 1,009,102",
		"<b>Самые ранние из полученных 3 тезиса:</b>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message missing %q:\n%s", want, got)
		}
	}
}

func TestFormatSubMinuteAge(t *testing.T) {
	a := sample()
	a.CreatedAt = a.DetectedAt.Add(-30 * time.Second)
	a.RateWindow = 20 * time.Second
	got := Format(a, msk)
	for _, want := range []string{
		"создан &lt;1 мин назад",
		"Тезисов в минуту: 9 (3 за &lt;1 мин)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "<1") {
		t.Errorf("raw <1 is an invalid Telegram tag:\n%s", got)
	}
}

func TestVolumeLine(t *testing.T) {
	a := sample()
	a.VolumeAt = a.DetectedAt.Add(-5 * time.Minute)
	a.Volume = domain.Volume{USD1h: 3000}
	if got := VolumeLine(a); got != "Объём: 1ч $3K · 5м $0 (замер 5 мин назад)" {
		t.Errorf("stale line = %q", got)
	}
	a.VolumeAt = time.Time{}
	if got := Format(a, msk); strings.Contains(got, "Объём") {
		t.Errorf("no sample, no volume line:\n%s", got)
	}
}

func TestPluralMoneyDuration(t *testing.T) {
	cases := map[int]string{1: "тезис", 2: "тезиса", 4: "тезиса", 5: "тезисов", 11: "тезисов", 12: "тезисов", 21: "тезис", 22: "тезиса", 111: "тезисов"}
	for n, want := range cases {
		if got := PluralTheses(n); got != want {
			t.Errorf("PluralTheses(%d) = %q, want %q", n, got, want)
		}
	}
	money := map[float64]string{950: "$950", 45_300: "$45.3K", 800_000: "$800K", 999_950: "$1M", 1_200_000: "$1.2M", 2_500_000_000: "$2.5B"}
	for v, want := range money {
		if got := CompactUSD(v); got != want {
			t.Errorf("CompactUSD(%v) = %q, want %q", v, got, want)
		}
	}
	if got := FullUSD(1234567.4); got != "$1,234,567" {
		t.Errorf("FullUSD = %q", got)
	}
	durations := map[time.Duration]string{30 * time.Second: "<1 мин", 42 * time.Minute: "42 мин", 2 * time.Hour: "2 ч",
		3*time.Hour + 5*time.Minute: "3 ч 5 мин", 50 * time.Hour: "2 д 2 ч"}
	for d, want := range durations {
		if got := humanDuration(d); got != want {
			t.Errorf("humanDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
