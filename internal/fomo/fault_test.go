package fomo

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"fomobot/internal/session"
)

func TestRetryableAndAuth(t *testing.T) {
	cases := []struct {
		err   error
		retry bool
		auth  bool
	}{
		{errors.New("proxy down"), true, false},
		{fmt.Errorf("get access token: %w", session.ErrNoToken), true, true},
		{fmt.Errorf("wrap: %w", ErrUnauthorized), true, true},
		{&HTTPError{Status: http.StatusTooManyRequests}, true, false},
		{&HTTPError{Status: 503}, true, false},
		{&HTTPError{Status: http.StatusBadRequest}, false, false},
	}
	for _, c := range cases {
		if got := Retryable(c.err); got != c.retry {
			t.Errorf("Retryable(%v) = %v, want %v", c.err, got, c.retry)
		}
		if got := AuthRejected(c.err); got != c.auth {
			t.Errorf("AuthRejected(%v) = %v, want %v", c.err, got, c.auth)
		}
	}
}
