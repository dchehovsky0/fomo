package fomo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Transport interface {
	FetchTheses(ctx context.Context, accessToken string, query ThesisQuery) (BrowserHTTPResponse, error)
}

type Client struct{ Transport Transport }

// wireEnvelope keeps responseObject raw first. Fomo error envelopes may contain
// responseObject: [] while successful thesis responses contain responseObject: {...}.
type wireEnvelope struct {
	Success        bool            `json:"success"`
	Message        string          `json:"message"`
	ResponseObject json.RawMessage `json:"responseObject"`
	StatusCode     int             `json:"statusCode"`
	Error          string          `json:"error"`
}

func (c *Client) GetTheses(ctx context.Context, accessToken string, query ThesisQuery) (Result, error) {
	started := time.Now().UTC()
	raw, err := c.Transport.FetchTheses(ctx, accessToken, query)
	received := time.Now().UTC()
	browserFetchStarted := parseWireTime(raw.FetchStartedAt)
	browserHeadersReceived := parseWireTime(raw.HeadersReceivedAt)
	browserBodyRead := parseWireTime(raw.BodyReadAt)
	base := Result{
		RequestStartedAt:         started,
		BrowserFetchStartedAt:    browserFetchStarted,
		BrowserHeadersReceivedAt: browserHeadersReceived,
		BrowserBodyReadAt:        browserBodyRead,
		ReceivedAt:               received,
		Latency:                  received.Sub(started),
		Query:                    query,
	}
	if err != nil {
		return base, &RequestError{Kind: ErrNetwork, Message: err.Error()}
	}

	var env wireEnvelope
	var envErr error
	if strings.TrimSpace(raw.Body) != "" {
		envErr = json.Unmarshal([]byte(raw.Body), &env)
	}

	feed := FeedResponse{
		Success:    env.Success,
		Message:    env.Message,
		StatusCode: env.StatusCode,
		Error:      env.Error,
	}
	res := Result{
		Feed:                     feed,
		HTTPStatus:               raw.HTTPStatus,
		RetryAfter:               raw.RetryAfter,
		ServerDate:               raw.Date,
		RequestStartedAt:         started,
		BrowserFetchStartedAt:    browserFetchStarted,
		BrowserHeadersReceivedAt: browserHeadersReceived,
		BrowserBodyReadAt:        browserBodyRead,
		ReceivedAt:               received,
		Latency:                  received.Sub(started),
		Query:                    query,
	}
	bodyStatus := env.StatusCode

	if raw.HTTPStatus == 429 || bodyStatus == 429 {
		return res, &RequestError{Kind: ErrRateLimit, HTTPStatus: raw.HTTPStatus, BodyStatus: bodyStatus, RetryAfter: raw.RetryAfter, Message: envelopeMessage(env, raw.Body)}
	}
	if raw.HTTPStatus == 401 || raw.HTTPStatus == 430 || raw.HTTPStatus == 431 || bodyStatus == 401 || bodyStatus == 430 || bodyStatus == 431 {
		return res, &RequestError{Kind: ErrAuth, HTTPStatus: raw.HTTPStatus, BodyStatus: bodyStatus, RetryAfter: raw.RetryAfter, Message: envelopeMessage(env, raw.Body)}
	}
	if raw.HTTPStatus == 403 || bodyStatus == 403 {
		return res, &RequestError{Kind: ErrForbidden, HTTPStatus: raw.HTTPStatus, BodyStatus: bodyStatus, RetryAfter: raw.RetryAfter, Message: envelopeMessage(env, raw.Body)}
	}
	if raw.HTTPStatus < 200 || raw.HTTPStatus >= 300 {
		return res, &RequestError{Kind: ErrAPI, HTTPStatus: raw.HTTPStatus, BodyStatus: bodyStatus, RetryAfter: raw.RetryAfter, Message: envelopeMessage(env, raw.Body)}
	}
	if bodyStatus != 0 && (bodyStatus < 200 || bodyStatus >= 300) {
		return res, &RequestError{Kind: ErrAPI, HTTPStatus: raw.HTTPStatus, BodyStatus: bodyStatus, RetryAfter: raw.RetryAfter, Message: envelopeMessage(env, raw.Body)}
	}
	if envErr != nil {
		return res, &RequestError{Kind: ErrParse, HTTPStatus: raw.HTTPStatus, RetryAfter: raw.RetryAfter, Message: fmt.Sprintf("decode envelope: %v; body=%q", envErr, truncate(raw.Body, 512))}
	}

	trimmed := bytes.TrimSpace(env.ResponseObject)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return res, nil
	}
	if trimmed[0] != '{' {
		return res, &RequestError{Kind: ErrParse, HTTPStatus: raw.HTTPStatus, BodyStatus: bodyStatus, RetryAfter: raw.RetryAfter, Message: fmt.Sprintf("unexpected responseObject shape: %s", truncate(string(trimmed), 256))}
	}
	if err := json.Unmarshal(trimmed, &res.Feed.ResponseObject); err != nil {
		return res, &RequestError{Kind: ErrParse, HTTPStatus: raw.HTTPStatus, BodyStatus: bodyStatus, RetryAfter: raw.RetryAfter, Message: fmt.Sprintf("decode responseObject: %v; body=%q", err, truncate(raw.Body, 512))}
	}
	return res, nil
}

func ParseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if sec, err := strconv.Atoi(v); err == nil && sec >= 0 {
		return time.Duration(sec) * time.Second, true
	}
	if t, err := time.Parse(time.RFC1123, v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d, true
		}
	}
	return 0, false
}

func envelopeMessage(f wireEnvelope, raw string) string {
	for _, s := range []string{f.Error, f.Message} {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return truncate(raw, 512)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func parseWireTime(v string) time.Time {
	if strings.TrimSpace(v) == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
