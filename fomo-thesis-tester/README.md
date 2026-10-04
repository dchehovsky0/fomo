# Fomo Thesis Tester v7 — latency/stability run

This build is for the second controlled run: 5 already-authenticated Chrome/Fomo sessions, 24 tokens, 3h30m.

## What changed from v6

- Endpoint changed to `GET /feed/token/sortedThesis`.
- Live query window: last 5 minutes, `limit=500`, `threshold=0`.
- Every 10th poll of a token is a 30-minute audit window. It does **not** add an extra request; it replaces that token's normal 5-minute window for that poll. This is a safety net for a thesis that becomes visible unusually late.
- Token scheduling is deterministic round-robin, not random. With 24 tokens and one global request every 2 seconds, each token should be requested about once every 48 seconds.
- Accounts are also selected round-robin when healthy/available.
- Deduplication is account-independent. Fomo can expose different `thesis_id` values for the same logical thesis on different accounts, so v7 uses a SHA-256 canonical identity derived from network, token, backend `createdAt`, author identity and comment text.
- `context canceled` / `context deadline exceeded` are no longer treated as an authentication failure or a reason to put an account into auth cooldown.
- Requests use `cache: no-store`.
- The run can be overridden from CLI with `-duration=3h30m` without editing your current `config.json`.

## Timing logs

For every request:

- `poll_started`: scheduler time, dispatch time, query window and account.
- `poll_ok`: Go dispatch/start plus browser-side `fetchStartedAt`, `headersReceivedAt`, `bodyReadAt`, final Go receive time, server `Date` when exposed by CORS, item count, `hasNextPage`, and whether the requested time window was complete. This separates network-to-headers, body-read, and CDP-return overhead.
- `poll_failed`: same request identity/timestamps plus network/API/auth error classification.
- `query_window_saturated`: warns when `hasNextPage=true` or the 500-row limit is hit. Latency conclusions from an incomplete window must be treated cautiously.

For every genuinely new logical thesis:

- `created_at_backend`: Fomo's `createdAt`.
- `first_seen_request_started_at`: when our request that first returned it started.
- `first_seen_browser_headers_received_at`: when browser `fetch()` resolved with response headers.
- `first_seen_browser_body_read_at`: when the JSON body containing the thesis finished reading in the browser.
- `go_received_at`: when the evaluated result returned through CDP to Go.
- `detection_delay_ms = go_received_at - createdAt` (actual alert-side delay).
- `backend_returned_upper_bound_ms = browser_body_read_at - createdAt` (tighter upper bound before CDP/Go overhead).
- `first_poll_after_created_at_*`: earliest prior successful poll whose requested time window already covered the thesis timestamp.
- `first_poll_wait_ms`: how long the scheduler took to make the first eligible request after creation.
- `eligible_successful_polls_before_seen`: how many successful eligible responses came back before Fomo finally returned the thesis.
- If a previous **complete** query window covered the thesis timestamp but did not return it, v7 logs:
  - `last_absent_complete_poll_received_at`
  - `backend_lag_lower_bound_ms`
  - `backend_lag_upper_bound_ms`
  - `backend_visibility_window_ms`

That gives us the strongest timing statement possible from polling. We cannot observe the exact internal instant Fomo indexed a thesis between two requests, but when complete windows are available we can bracket it to:

`(last complete response where thesis was absent, first response where thesis was present]`.

## Run

Keep the same five normal Chrome sessions running on ports `9221`–`9225`. Do not relogin if they are still authenticated.

Your existing `config.json`, `tokens.json`, `profiles/`, `logs/` and `go.sum` are intentionally not included in this archive. Extract the archive over the current project.

Check compile:

```bash
go test ./...
```

Run for exactly 3.5 hours even if your old config still says 3h:

```bash
go run ./cmd/fomo \
  -mode=run \
  -config=config.json \
  -tokens=tokens.json \
  -duration=3h30m
```

Expected startup line:

```text
scheduler_started tokens=24 accounts=5 interval=2s mode=round_robin endpoint=/feed/token/sortedThesis limit=500 ... expected_token_poll_interval=48s
```

## End-of-run summaries

On shutdown v7 writes:

- `run_summary`
- one `account_run_summary` per account
- one `token_run_summary` per token, including `max_poll_start_gap_ms`

Send the entire new JSONL file after the run. The useful final analysis is:

1. request success rate and 401/403/429/network errors;
2. access-token rotations and recovery behavior;
3. actual per-token polling cadence and gaps;
4. HTTP latency p50/p95/p99;
5. thesis detection delay p50/p95/p99;
6. scheduler wait vs backend-visibility delay;
7. how often a thesis was absent from one or more complete eligible responses before appearing;
8. any 5-minute window saturation and any thesis discovered only by the 30-minute audit window;
9. per-account stability and skew.

## Why sortedThesis

Community reverse-engineering currently documents `/feed/token/sortedThesis` with `tokenAddress`, `networkId`, `afterTime`, `beforeTime`, `limit`, and `threshold`, with timestamps in milliseconds. We already observed that this endpoint accepts larger limits than the default thesis feed; this test uses 500 as the configured cap. The code logs `hasNextPage`/limit saturation instead of assuming that 500 always means complete coverage.
