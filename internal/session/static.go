package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// Static serves a token copied manually from the browser: either inline
// (FOMO_ACCESS_TOKEN) or from a file that is re-read whenever it changes, so
// the token can be replaced without restarting the bot.
type Static struct {
	inline string
	file   string
	log    *slog.Logger

	mu      sync.Mutex
	token   string
	exp     time.Time
	fileMod time.Time
	warned  bool
}

func NewStatic(inline, file string, log *slog.Logger) *Static {
	s := &Static{inline: inline, file: file, log: log}
	if inline != "" {
		s.set(inline)
	}
	return s
}

func (s *Static) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.file != "" {
		if err := s.reloadFile(); err != nil {
			return "", err
		}
	}
	if s.token == "" {
		return "", fmt.Errorf("%w: set FOMO_ACCESS_TOKEN or session.token_file", ErrNoToken)
	}
	if !s.exp.IsZero() && time.Now().After(s.exp) {
		return "", fmt.Errorf("%w: token expired at %s, copy a fresh privy:token from the browser",
			ErrNoToken, s.exp.Local().Format(time.DateTime))
	}
	if left := time.Until(s.exp); !s.exp.IsZero() && left < 10*time.Minute && !s.warned {
		s.warned = true
		s.log.Warn("static fomo token expires soon", "expires_in", left.Round(time.Second))
	}
	return s.token, nil
}

func (s *Static) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fileMod = time.Time{}
	s.log.Warn("fomo rejected the access token; replace it manually (static session mode)")
}

func (s *Static) reloadFile() error {
	st, err := os.Stat(s.file)
	if err != nil {
		if s.token != "" || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read token file: %w", err)
	}
	if st.ModTime().Equal(s.fileMod) && s.token != "" {
		return nil
	}
	data, err := os.ReadFile(s.file)
	if err != nil {
		return fmt.Errorf("read token file: %w", err)
	}
	s.fileMod = st.ModTime()
	if tok := Normalize(string(data)); tok != "" && tok != s.token {
		s.set(tok)
		s.log.Info("loaded fomo token from file", "file", s.file, "expires", s.exp.Local().Format(time.DateTime))
	}
	return nil
}

func (s *Static) set(raw string) {
	s.token = Normalize(raw)
	s.exp, _ = ParseExpiry(s.token)
	s.warned = false
}
