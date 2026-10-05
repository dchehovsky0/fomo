package web

import (
	_ "embed"
	"net/http"
	"strings"
)

// adminCookie keeps the operator token out of the board address after the
// first open, so a copied link and the Referer of an outbound click do not
// carry it. The health routes accept the same cookie.
const adminCookie = "fomo_admin"

//go:embed board.html
var boardHTML []byte

// board serves the watch list. A valid ?token= is stored in the cookie and
// the browser is sent to / without it. requireAdmin has already checked it.
func (s *Server) board(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("token") != "" {
		http.SetCookie(w, &http.Cookie{
			Name: adminCookie, Value: r.URL.Query().Get("token"), Path: "/",
			HttpOnly: true, SameSite: http.SameSiteLaxMode,
			Secure: strings.HasPrefix(s.cfg.PublicURL, "https://"),
		})
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	// connect-src is this server only: the page polls /api/board.
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; img-src https: http: data:; connect-src 'self'; base-uri 'none'; form-action 'none'")
	w.Write(boardHTML)
}
