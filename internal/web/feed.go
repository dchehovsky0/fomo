package web

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"fomobot/internal/notify"
)

// Feed is the alerts whose theses pages are still on disk.
type Feed struct {
	AsOf   time.Time  `json:"as_of"`
	Alerts []FeedItem `json:"alerts"`
}

// FeedItem is one sent alert. MarketCap is the cap at the moment the alert
// was assembled. PageURL is the theses page on this server.
type FeedItem struct {
	ID        string       `json:"id"`
	At        time.Time    `json:"at"`
	Kind      string       `json:"kind"`
	Mint      string       `json:"mint"`
	Symbol    string       `json:"symbol"`
	Name      string       `json:"name"`
	Dex       string       `json:"dex,omitempty"`
	Theses    int          `json:"theses"`
	MarketCap float64      `json:"market_cap"`
	PageURL   string       `json:"page_url"`
	FomoURL   string       `json:"fomo_url,omitempty"`
	AxiomURL  string       `json:"axiom_url,omitempty"`
	Chain     string       `json:"chain,omitempty"`
	Tier      int          `json:"tier,omitempty"`
	ImageURL  string       `json:"image_url,omitempty"`
	Snippets  []FeedThesis `json:"snippets,omitempty"`
}

// FeedThesis is one thesis shown in the feed row. Newest first.
type FeedThesis struct {
	At     time.Time `json:"at"`
	Text   string    `json:"text"`
	Handle string    `json:"handle,omitempty"`
}

// feedSnap remembers a parsed page so a poll does not reread every file.
type feedSnap struct {
	mod  time.Time
	size int64
	item FeedItem
}

// Feed reads the saved alert pages. Files that have not changed since the
// previous call are not parsed again.
func (s *Server) Feed(now time.Time) Feed {
	s.feedMu.Lock()
	defer s.feedMu.Unlock()
	out := Feed{AsOf: now, Alerts: []FeedItem{}}
	entries, err := os.ReadDir(s.cfg.Dir)
	if err != nil {
		s.log.Warn("list alert pages", "err", err)
		return out
	}
	if s.feedCache == nil {
		s.feedCache = map[string]feedSnap{}
	}
	hidden := s.dismissedSet()
	next := make(map[string]feedSnap, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		if !idPattern.MatchString(id) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		var item FeedItem
		if snap, ok := s.feedCache[id]; ok && snap.size == info.Size() && snap.mod.Equal(info.ModTime()) {
			next[id] = snap
			item = snap.item
		} else {
			var ok bool
			item, ok = s.readAlert(id)
			if !ok {
				continue
			}
			next[id] = feedSnap{mod: info.ModTime(), size: info.Size(), item: item}
		}
		if _, skip := hidden[id]; skip {
			continue
		}
		out.Alerts = append(out.Alerts, item)
	}
	s.feedCache = next
	slices.SortFunc(out.Alerts, func(a, b FeedItem) int {
		if n := b.At.Compare(a.At); n != 0 {
			return n
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

func (s *Server) readAlert(id string) (FeedItem, bool) {
	data, err := os.ReadFile(filepath.Join(s.cfg.Dir, id+".json"))
	if err != nil {
		return FeedItem{}, false
	}
	var a notify.Alert
	if err := json.Unmarshal(data, &a); err != nil {
		s.log.Warn("alert page", "id", id, "err", err)
		return FeedItem{}, false
	}
	at := a.SentAt
	if at.IsZero() {
		at = a.DetectedAt
	}
	if at.IsZero() {
		return FeedItem{}, false
	}
	return FeedItem{
		ID: id, At: at, Kind: a.Kind, Mint: a.Token, Symbol: a.Symbol, Name: a.Name, Dex: a.Dex,
		Theses: a.Count, MarketCap: a.MarketCap, PageURL: "/t/" + id,
		FomoURL: a.FomoURL, AxiomURL: a.AxiomURL, Chain: a.Chain, Tier: a.Tier, ImageURL: a.ImageURL,
		Snippets: feedSnippets(a),
	}, true
}

// feedSnippets is the three newest thesis texts saved with the alert.
// Latest is that set. Older pages only have the first theses, and those are used.
func feedSnippets(a notify.Alert) []FeedThesis {
	src := a.Latest
	if len(src) == 0 {
		src = a.First
	}
	if len(src) > 3 {
		src = src[len(src)-3:]
	}
	out := make([]FeedThesis, 0, len(src))
	for i := len(src) - 1; i >= 0; i-- {
		text := strings.Join(strings.Fields(src[i].Text), " ")
		if text == "" {
			continue
		}
		out = append(out, FeedThesis{At: src[i].At, Text: text, Handle: src[i].Handle})
	}
	return out
}

// TokenDetail is one opened call: the token and the theses saved with it.
type TokenDetail struct {
	ID        string         `json:"id"`
	At        time.Time      `json:"at"`
	Mint      string         `json:"mint"`
	Symbol    string         `json:"symbol"`
	Name      string         `json:"name"`
	Chain     string         `json:"chain,omitempty"`
	Dex       string         `json:"dex,omitempty"`
	Tier      int            `json:"tier,omitempty"`
	ImageURL  string         `json:"image_url,omitempty"`
	MarketCap float64        `json:"market_cap"`
	Count     int            `json:"count"`
	CreatedAt time.Time      `json:"created_at,omitzero"`
	FomoURL   string         `json:"fomo_url,omitempty"`
	AxiomURL  string         `json:"axiom_url,omitempty"`
	PageURL   string         `json:"page_url"`
	Theses    []DetailThesis `json:"theses"`
}

// DetailThesis is one thesis on the open token page.
type DetailThesis struct {
	At          time.Time `json:"at"`
	Text        string    `json:"text"`
	Handle      string    `json:"handle,omitempty"`
	AvatarURL   string    `json:"avatar_url,omitempty"`
	Verified    bool      `json:"verified,omitempty"`
	IsDev       bool      `json:"is_dev,omitempty"`
	Closed      bool      `json:"closed,omitempty"`
	PositionUSD float64   `json:"position_usd"`
	MarketCap   float64   `json:"market_cap,omitempty"`
}

func (s *Server) readToken(id string) (TokenDetail, bool) {
	data, err := os.ReadFile(filepath.Join(s.cfg.Dir, id+".json"))
	if err != nil {
		return TokenDetail{}, false
	}
	var a notify.Alert
	if err := json.Unmarshal(data, &a); err != nil {
		s.log.Warn("alert page", "id", id, "err", err)
		return TokenDetail{}, false
	}
	at := a.SentAt
	if at.IsZero() {
		at = a.DetectedAt
	}
	if at.IsZero() {
		return TokenDetail{}, false
	}
	return TokenDetail{
		ID: id, At: at, Mint: a.Token, Symbol: a.Symbol, Name: a.Name, Chain: a.Chain, Dex: a.Dex,
		Tier: a.Tier, ImageURL: a.ImageURL, MarketCap: a.MarketCap, Count: a.Count, CreatedAt: a.CreatedAt,
		FomoURL: a.FomoURL, AxiomURL: a.AxiomURL, PageURL: "/t/" + id, Theses: detailTheses(a),
	}, true
}

// detailTheses is the list saved for the open page. Older alerts only have
// the first and latest theses, and those are combined.
func detailTheses(a notify.Alert) []DetailThesis {
	src := a.Theses
	if len(src) == 0 {
		src = append(append([]notify.Thesis{}, a.First...), a.Latest...)
	}
	seen := map[string]bool{}
	out := []DetailThesis{}
	for _, t := range src {
		text := strings.Join(strings.Fields(t.Text), " ")
		key := t.At.UTC().Format(time.RFC3339Nano) + "\n" + t.Handle + "\n" + text
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, DetailThesis{
			At: t.At, Text: text, Handle: t.Handle, AvatarURL: t.AvatarURL,
			Verified: t.Verified, IsDev: t.IsDev, Closed: t.Closed,
			PositionUSD: t.PositionUSD, MarketCap: t.MarketCap,
		})
	}
	return out
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idPattern.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	d, ok := s.readToken(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	body, err := json.Marshal(d)
	if err != nil {
		http.Error(w, "token", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.Write(body)
}

func (s *Server) dismissedPath() string {
	return filepath.Join(s.cfg.Dir, "dismissed.json")
}

// dismissedSet is the calls hidden from the feed. The theses page stays.
// Caller holds feedMu.
func (s *Server) dismissedSet() map[string]struct{} {
	if s.dismissed != nil {
		return s.dismissed
	}
	s.dismissed = map[string]struct{}{}
	data, err := os.ReadFile(s.dismissedPath())
	if err != nil {
		return s.dismissed
	}
	var ids []string
	if err := json.Unmarshal(data, &ids); err != nil {
		s.log.Warn("dismissed list", "err", err)
		return s.dismissed
	}
	for _, id := range ids {
		if idPattern.MatchString(id) {
			s.dismissed[id] = struct{}{}
		}
	}
	return s.dismissed
}

func (s *Server) writeDismissed() error {
	ids := make([]string, 0, len(s.dismissed))
	for id := range s.dismissed {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	data, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	path := s.dismissedPath()
	if err := os.WriteFile(path+".tmp", data, 0o644); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// dismissAlert hides id from the feed. The theses page is left in place.
func (s *Server) dismissAlert(id string) error {
	if _, err := os.Stat(filepath.Join(s.cfg.Dir, id+".json")); err != nil {
		return err
	}
	s.feedMu.Lock()
	defer s.feedMu.Unlock()
	set := s.dismissedSet()
	if _, ok := set[id]; ok {
		return nil
	}
	set[id] = struct{}{}
	return s.writeDismissed()
}

func (s *Server) dismiss(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idPattern.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	if err := s.dismissAlert(id); err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		s.log.Warn("dismiss alert", "id", id, "err", err)
		http.Error(w, "dismiss", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) alerts(w http.ResponseWriter, _ *http.Request) {
	body, err := json.Marshal(s.Feed(time.Now()))
	if err != nil {
		http.Error(w, "alerts", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.Write(body)
}
