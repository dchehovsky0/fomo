package fomo

import (
	"errors"
	"net/http"

	"fomobot/internal/session"
)

// Retryable reports that another fomo account may succeed: the token was
// rejected, there is no login, the proxy or network failed, or fomo answered
// 429 or 5xx. A 4xx about the request itself is not retryable.
func Retryable(err error) bool {
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Status == http.StatusTooManyRequests || he.Status >= 500
	}
	return true
}

// AuthRejected reports that the account's token was refused or is missing,
// which deserves a longer rest than a network blip.
func AuthRejected(err error) bool {
	return errors.Is(err, ErrUnauthorized) || errors.Is(err, session.ErrNoToken)
}
