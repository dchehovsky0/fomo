// Package web serves the page with the full texts of an alert's theses; the
// Telegram message links to it with a button.
package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"fomobot/internal/notify"
)

//go:embed page.html
var pageHTML string

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

type Config struct {
	Dir string
	// PublicURL is how users reach this server, e.g. https://bot.example.com.
	PublicURL string
	Keep      time.Duration
	// Exposed: the server is reachable from other machines. Admin routes
	// then require AdminToken; otherwise they answer only loopback clients.
	Exposed    bool
	AdminToken string
}

type Server struct {
	cfg   Config
	loc   *time.Location
	tmpl  *template.Template
	log   *slog.Logger
	admin map[string]http.Handler

	feedMu    sync.Mutex
	feedCache map[string]feedSnap
	// dismissed is nil until the first read of dismissed.json.
	dismissed map[string]struct{}
}

func New(cfg Config, loc *time.Location, log *slog.Logger) (*Server, error) {
	if cfg.Dir == "" {
		return nil, errors.New("web: pages dir is required")
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, err
	}
	if loc == nil {
		loc = time.Local
	}
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	s := &Server{cfg: cfg, loc: loc, log: log, admin: map[string]http.Handler{}}
	tmpl, err := template.New("page").Funcs(template.FuncMap{
		"usd":     notify.FullUSD,
		"compact": notify.CompactUSD,
		"group":   notify.GroupThousands,
		"inc":     func(i int) int { return i + 1 },
		"when":    func(t time.Time) string { return t.In(s.loc).Format("02.01.2006 15:04:05") },
	}).Parse(pageHTML)
	if err != nil {
		return nil, fmt.Errorf("web: parse page template: %w", err)
	}
	s.tmpl = tmpl
	return s, nil
}

// Save stores the alert and returns the URL of its page.
func (s *Server) Save(a notify.Alert) (string, error) {
	if s.cfg.PublicURL == "" {
		return "", errors.New("web: public_url is not set")
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(b)
	data, err := json.Marshal(a)
	if err != nil {
		return "", err
	}
	path := filepath.Join(s.cfg.Dir, id+".json")
	if err := os.WriteFile(path+".tmp", data, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return "", err
	}
	url := s.cfg.PublicURL + "/t/" + id
	s.log.Info("theses page saved", "token", a.Token, "url", url, "bytes", len(data))
	return url, nil
}

// Discard removes a page that was saved before Telegram accepted the alert.
// The feed then does not show a call that never went out. A second call is a
// no-op, and anything that is not a page id is left untouched.
func (s *Server) Discard(pageURL string) error {
	id := pageID(pageURL)
	if !idPattern.MatchString(id) {
		return nil
	}
	path := filepath.Join(s.cfg.Dir, id+".json")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.feedMu.Lock()
	delete(s.feedCache, id)
	s.feedMu.Unlock()
	s.log.Info("theses page removed after a failed send", "id", id)
	return nil
}

func pageID(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	path := strings.TrimRight(u.Path, "/")
	const marker = "/t/"
	i := strings.LastIndex(path, marker)
	if i < 0 {
		return ""
	}
	return path[i+len(marker):]
}

// HandleAdmin registers a route for the bot's operator; call it before Run.
func (s *Server) HandleAdmin(pattern string, h http.Handler) {
	s.admin[pattern] = h
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /t/{id}", s.page)
	mux.Handle("GET /{$}", s.requireAdmin(http.HandlerFunc(s.board)))
	mux.Handle("GET /api/alerts", s.requireAdmin(http.HandlerFunc(s.alerts)))
	mux.Handle("GET /api/alerts/{id}", s.requireAdmin(http.HandlerFunc(s.token)))
	mux.Handle("POST /api/alerts/{id}/dismiss", s.requireAdmin(http.HandlerFunc(s.dismiss)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	for pattern, h := range s.admin {
		mux.Handle(pattern, s.requireAdmin(h))
	}
	return mux
}

func (s *Server) requireAdmin(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")
		if s.cfg.AdminToken != "" {
			got := r.URL.Query().Get("token")
			if v, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
				got = v
			}
			if got == "" {
				if c, err := r.Cookie(adminCookie); err == nil {
					got = c.Value
				}
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.AdminToken)) != 1 {
				http.Error(w, "нужен токен: ?token=FOMO_ADMIN_TOKEN или заголовок Authorization: Bearer", http.StatusUnauthorized)
				return
			}
		} else if s.cfg.Exposed {
			http.Error(w, "задай FOMO_ADMIN_TOKEN, чтобы открыть эту страницу на сервере", http.StatusForbidden)
			return
		} else if host, _, err := net.SplitHostPort(r.RemoteAddr); err != nil || !net.ParseIP(host).IsLoopback() {
			http.Error(w, "доступно только с этого компьютера", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

type view struct {
	A          notify.Alert
	Headline   string
	ThesesWord string
	Initial    string
	Age        string
	Rate       string
	Recent     string
	Window     string
	Zone       string
}

// Since is how long after the token launch t was, empty when unknown.
func (v view) Since(t time.Time) string {
	if v.A.CreatedAt.IsZero() || t.Before(v.A.CreatedAt) {
		return ""
	}
	return notify.HumanDuration(t.Sub(v.A.CreatedAt))
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	id := r.PathValue("id")
	status := http.StatusOK
	defer func() {
		s.log.Info("theses page served", "id", id, "status", status, "took", time.Since(started).Round(time.Millisecond))
	}()
	if !idPattern.MatchString(id) {
		status = http.StatusNotFound
		http.NotFound(w, r)
		return
	}
	data, err := os.ReadFile(filepath.Join(s.cfg.Dir, id+".json"))
	if err != nil {
		status = http.StatusNotFound
		http.Error(w, "Страница не найдена или устарела", http.StatusNotFound)
		return
	}
	var a notify.Alert
	if err := json.Unmarshal(data, &a); err != nil {
		status = http.StatusInternalServerError
		http.Error(w, "broken page", http.StatusInternalServerError)
		return
	}
	v := view{
		A: a, Headline: notify.Headline(a), ThesesWord: notify.PluralTheses(len(a.First)),
		Initial: initial(a.Symbol), Rate: notify.RatePerMinute(a), Window: notify.HumanDuration(a.RateWindow),
		Zone: s.loc.String(),
	}
	if v.Zone == "Local" {
		v.Zone = time.Now().In(s.loc).Format("MST")
	}
	v.Recent = notify.GroupThousands(a.Recent)
	if a.RecentCapped {
		v.Recent = "≥" + v.Recent
	}
	if !a.CreatedAt.IsZero() {
		v.Age = notify.HumanDuration(a.DetectedAt.Sub(a.CreatedAt))
	}
	var body bytes.Buffer
	if err := s.tmpl.Execute(&body, v); err != nil {
		status = http.StatusInternalServerError
		s.log.Warn("render theses page", "id", id, "took", time.Since(started).Round(time.Millisecond), "err", err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "private, max-age=300")
	h.Set("X-Robots-Tag", "noindex, nofollow")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; img-src https: http: data:; style-src 'unsafe-inline'; script-src 'unsafe-inline'; base-uri 'none'; form-action 'none'")
	w.Write(body.Bytes())
}

// Run serves until ctx is done and deletes pages older than Keep every hour.
func (s *Server) Run(ctx context.Context, listen string) error {
	srv := &http.Server{Addr: listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	s.log.Info("theses pages server started", "listen", listen, "public_url", s.cfg.PublicURL)

	t := time.NewTicker(time.Hour)
	defer t.Stop()
	s.prune()
	for {
		select {
		case err := <-errc:
			return fmt.Errorf("web server: %w", err)
		case <-t.C:
			s.prune()
		case <-ctx.Done():
			shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return srv.Shutdown(shut)
		}
	}
}

func (s *Server) prune() {
	if s.cfg.Keep <= 0 {
		return
	}
	entries, err := os.ReadDir(s.cfg.Dir)
	if err != nil {
		s.log.Warn("list theses pages", "err", err)
		return
	}
	limit := time.Now().Add(-s.cfg.Keep)
	for _, e := range entries {
		info, err := e.Info()
		id := strings.TrimSuffix(e.Name(), ".json")
		if err != nil || e.IsDir() || !idPattern.MatchString(id) || info.ModTime().After(limit) {
			continue
		}
		os.Remove(filepath.Join(s.cfg.Dir, e.Name()))
	}
}

func initial(symbol string) string {
	r, _ := utf8.DecodeRuneInString(symbol)
	if r == utf8.RuneError || !unicode.IsPrint(r) {
		return "?"
	}
	return string(unicode.ToUpper(r))
}
