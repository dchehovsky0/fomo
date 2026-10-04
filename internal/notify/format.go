package notify

import (
	"fmt"
	"html"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"fomobot/internal/domain"
	"fomobot/internal/fomo"
)

// The alert model lives in domain; these names keep the rendering package's
// callers compiling.
type (
	Alert  = domain.Alert
	Thesis = domain.Thesis
)

const (
	KindTheses   = domain.KindTheses
	KindTrending = domain.KindTrending
)

// ThesesFrom converts fomo theses, keeping their order.
func ThesesFrom(items []fomo.Thesis) []Thesis {
	out := make([]Thesis, 0, len(items))
	for _, t := range items {
		out = append(out, Thesis{
			At: t.CreatedAt, Handle: t.Handle, AvatarURL: t.AuthorImageURL, Verified: t.Verified, IsDev: t.IsDev,
			Closed: t.PositionClosed, PositionUSD: t.PositionUSD, MarketCap: t.MarketCapAtCreation, Text: t.Comment,
		})
	}
	return out
}

// Headline is the first line of the alert, without markup.
func Headline(a Alert) string {
	if a.Kind == KindTrending {
		s := "Новый токен в трендах fomo"
		if a.Returned {
			s = "Вернулся в тренды fomo"
		}
		if a.Rank > 0 {
			s += fmt.Sprintf(" · #%d", a.Rank)
		}
		return s
	}
	n := a.Count
	if !a.CountKnown || n == 0 {
		n = a.Threshold
	}
	return fmt.Sprintf("%d %s на fomo", n, PluralTheses(n))
}

// RatePerMinute renders the current theses rate, e.g. "0.3" or "≥50".
func RatePerMinute(a Alert) string {
	minutes := a.RateWindow.Minutes()
	if minutes <= 0 {
		return ""
	}
	s := trimFloat(float64(a.Recent) / minutes)
	if a.RecentCapped {
		s = "≥" + s
	}
	return s
}

// Format renders an alert as Telegram HTML; times are shown in loc.
func Format(a Alert, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	var b strings.Builder
	icon := "🚨"
	if a.Kind == KindTrending {
		icon = "🔥"
	}
	fmt.Fprintf(&b, "%s <b>%s</b>\n", icon, html.EscapeString(Headline(a)))

	symbol := a.Symbol
	if symbol == "" {
		symbol = shortMint(a.Token)
	}
	fmt.Fprintf(&b, "<b>$%s</b>", html.EscapeString(symbol))
	if a.Name != "" && a.Name != a.Symbol {
		fmt.Fprintf(&b, " · %s", html.EscapeString(a.Name))
	}
	b.WriteString("\n")
	if when := alertSentAt(a); !when.IsZero() {
		fmt.Fprintf(&b, "Алерт: %s\n", when.In(loc).Format("02.01.2006 15:04:05"))
	}

	chain := []string{"Chain: " + html.EscapeString(orDefault(a.Chain, "Solana"))}
	if a.Dex != "" {
		chain = append(chain, html.EscapeString(a.Dex))
	}
	if !a.CreatedAt.IsZero() {
		chain = append(chain, "создан "+humanDuration(a.DetectedAt.Sub(a.CreatedAt))+" назад")
	}
	b.WriteString(strings.Join(chain, " · ") + "\n")
	fmt.Fprintf(&b, "CA: <code>%s</code>\n", html.EscapeString(a.Token))
	if a.MarketCap > 0 {
		fmt.Fprintf(&b, "MC: %s\n", CompactUSD(a.MarketCap))
	}
	if a.RateKnown {
		fmt.Fprintf(&b, "Тезисов в минуту: %s (%s%d за %s)", RatePerMinute(a), capMark(a.RecentCapped), a.Recent, humanDuration(a.RateWindow))
		if a.CountKnown {
			fmt.Fprintf(&b, " · всего %s", groupThousands(int64(a.Count)))
		}
		b.WriteString("\n")
	} else if a.CountKnown {
		fmt.Fprintf(&b, "Тезисов всего: %s\n", groupThousands(int64(a.Count)))
	}

	if len(a.First) > 0 {
		title := fmt.Sprintf("Первые %d %s:", len(a.First), PluralTheses(len(a.First)))
		if !a.FirstExact {
			title = fmt.Sprintf("Самые ранние из полученных %d %s:", len(a.First), PluralTheses(len(a.First)))
		}
		fmt.Fprintf(&b, "\n<b>%s</b>\n", title)
		for i, t := range a.First {
			fmt.Fprintf(&b, "%d. %s — ", i+1, ThesisTime(t.At, a.DetectedAt, loc))
			switch {
			case t.Closed && t.PositionUSD < 1:
				b.WriteString("позиция закрыта")
			case t.Closed:
				b.WriteString(FullUSD(t.PositionUSD) + " (вышел)")
			default:
				b.WriteString(FullUSD(t.PositionUSD))
			}
			b.WriteString("\n")
		}
	}

	var links []string
	if a.FomoURL != "" {
		links = append(links, fmt.Sprintf(`<a href="%s">fomo</a>`, html.EscapeString(a.FomoURL)))
	}
	if a.AxiomURL != "" {
		links = append(links, fmt.Sprintf(`<a href="%s">Axiom</a>`, html.EscapeString(a.AxiomURL)))
	}
	if len(links) > 0 {
		b.WriteString("\n" + strings.Join(links, " | "))
	}
	return strings.TrimRight(b.String(), "\n")
}

// alertSentAt is when the message was assembled and sent. DetectedAt is the
// fallback for a text rendered before Send stamps the clock.
func alertSentAt(a Alert) time.Time {
	if !a.SentAt.IsZero() {
		return a.SentAt
	}
	return a.DetectedAt
}

// ThesisTime shows only the clock for theses of the same day as ref.
func ThesisTime(t, ref time.Time, loc *time.Location) string {
	t = t.In(loc)
	if ry, rm, rd := ref.In(loc).Date(); t.Year() == ry && t.Month() == rm && t.Day() == rd {
		return t.Format("15:04:05")
	}
	if t.Year() == ref.In(loc).Year() {
		return t.Format("02.01 15:04")
	}
	return t.Format("02.01.2006 15:04")
}

// SortByTime orders theses from the oldest.
func SortByTime(items []fomo.Thesis) []fomo.Thesis {
	items = slices.Clone(items)
	slices.SortStableFunc(items, func(a, b fomo.Thesis) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return items
}

func capMark(capped bool) string {
	if capped {
		return "≥"
	}
	return ""
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func trimFloat(v float64) string {
	if v >= 10 {
		return strconv.FormatFloat(math.Round(v), 'f', 0, 64)
	}
	return strings.TrimSuffix(strconv.FormatFloat(math.Round(v*10)/10, 'f', 1, 64), ".0")
}

func PluralTheses(n int) string { return plural(n, "тезис", "тезиса", "тезисов") }

func plural(n int, one, few, many string) string {
	n = int(math.Abs(float64(n)))
	n10, n100 := n%10, n%100
	switch {
	case n10 == 1 && n100 != 11:
		return one
	case n10 >= 2 && n10 <= 4 && (n100 < 12 || n100 > 14):
		return few
	default:
		return many
	}
}

// humanDuration formats 42m as "42 мин", 3h5m as "3 ч 5 мин", 30s as "<1 мин".
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return "<1 мин"
	}
	d = d.Round(time.Minute)
	days, h, m := int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour), int(d%time.Hour/time.Minute)
	switch {
	case days > 0 && h > 0:
		return fmt.Sprintf("%d д %d ч", days, h)
	case days > 0:
		return fmt.Sprintf("%d д", days)
	case h == 0:
		return fmt.Sprintf("%d мин", m)
	case m == 0:
		return fmt.Sprintf("%d ч", h)
	default:
		return fmt.Sprintf("%d ч %d мин", h, m)
	}
}

// HumanDuration is humanDuration for templates.
func HumanDuration(d time.Duration) string { return humanDuration(d) }

// CompactUSD formats 1_200_000 as "$1.2M", 800_000 as "$800K".
func CompactUSD(v float64) string {
	units := []struct {
		size   float64
		suffix string
	}{{1e9, "B"}, {1e6, "M"}, {1e3, "K"}}
	abs := math.Abs(v)
	round1 := func(x float64) float64 { return math.Round(x*10) / 10 }
	for i, u := range units {
		if abs < u.size {
			continue
		}
		num := round1(abs / u.size)
		// 999_950 rounds to "1000.0K": show it as "$1M" instead.
		if num >= 1000 && i > 0 {
			u = units[i-1]
			num = round1(abs / u.size)
		}
		s := strings.TrimSuffix(strconv.FormatFloat(num, 'f', 1, 64), ".0")
		if v < 0 {
			s = "-" + s
		}
		return "$" + s + u.suffix
	}
	return "$" + strconv.FormatFloat(math.Round(v), 'f', 0, 64)
}

// FullUSD formats 27982.8 as "$27,983".
func FullUSD(v float64) string {
	n := int64(math.Round(v))
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	return sign + "$" + groupThousands(n)
}

// groupThousands formats 1234567 as "1,234,567".
func groupThousands(n int64) string {
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	digits := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return sign + b.String()
}

// GroupThousands is groupThousands for templates.
func GroupThousands(n int) string { return groupThousands(int64(n)) }

func shortMint(m string) string {
	if len(m) <= 10 {
		return m
	}
	return m[:4] + "…" + m[len(m)-4:]
}
