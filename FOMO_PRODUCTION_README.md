# Fomo Thesis Service — Production Flow

## 1. Account model

Each Fomo account runs in its own persistent Chrome profile.

Example:

```bash
open -na "Google Chrome" --args   --user-data-dir="/path/profiles/acc01"   --remote-debugging-port=9221   "https://fomo.family/token"
```

Use separate ports per account:

```text
acc01 -> 9221
acc02 -> 9222
acc03 -> 9223
acc04 -> 9224
acc05 -> 9225
```

The initial Fomo login is done manually in the real Chrome browser.

Go does **not** launch Chrome through `chromedp.NewExecAllocator`.
It connects to the already-running browser over CDP:

```go
chromedp.NewRemoteAllocator(ctx, "http://127.0.0.1:9221")
```

CDP ports must only be exposed on localhost.

---

## 2. Auth ownership

The key rule:

```text
Chrome / Fomo / Privy = session owner
Go                   = access-token consumer
```

Go must **not**:

```text
- call Privy refresh endpoints directly
- use privy:refresh_token
- rotate the Privy session itself
```

This was the unstable approach that led to `403`.

The real Fomo page owns the Privy lifecycle and refreshes the session itself.

Go only observes the current Fomo access token.

Token priority:

```text
1. Authorization Bearer observed on a real request to prod-api.fomo.family
2. localStorage["privy:token"]
```

Do not use:

```text
privy:pat
privy:refresh_token
```

JWT may be decoded only to read `exp`.

Never write the raw token to logs.

---

## 3. Access-token refresh

Go does not refresh the Privy token directly.

Expected flow:

```text
current JWT approaches expiry
        ↓
account state = REFRESHING
        ↓
scheduler stops assigning new work to that account
        ↓
the real Fomo page / Privy SDK rotates the access token
        ↓
Go observes the new Bearer / privy:token
        ↓
account state = HEALTHY
```

If the Fomo page does not recover automatically:

```text
reload/navigate the real Fomo application page
→ wait for Privy/Fomo to hydrate
→ read the new access token
```

Important production requirement:

```text
refresh of one account must never block the global scheduler
```

Example:

```text
acc03 refreshing
→ use acc01 / acc02 / acc04 / acc05
```

---

## 4. Thesis endpoint

Use:

```http
GET https://prod-api.fomo.family/feed/token/sortedThesis
```

Parameters:

```text
tokenAddress=<TOKEN_ADDRESS>
networkId=1399811149
afterTime=<unix milliseconds>
beforeTime=<unix milliseconds>
limit=500
threshold=0
```

Recommended live window:

```text
afterTime  = now - 5 minutes
beforeTime = now
limit      = 500
threshold  = 0
```

Example:

```text
/feed/token/sortedThesis
?tokenAddress=ABC...
&networkId=1399811149
&afterTime=1790940000000
&beforeTime=1790940300000
&limit=500
&threshold=0
```

---

## 5. Browser-side request

The tested version performs the request inside the real browser session:

```js
fetch(url, {
    method: "GET",
    credentials: "include",
    headers: {
        "Accept": "application/json",
        "Authorization": "Bearer " + accessToken,
        "X-Supported-Chains": "1,56,143,4663,8453,1399811149"
    }
})
```

When calling browser JavaScript from Go/CDP, the returned Promise must be awaited:

```text
awaitPromise = true
```

After receiving the response, sort thesis items manually by:

```text
createdAt ASC
```

---

## 6. Polling window

Recommended production defaults:

```text
live lookback: 5 minutes
limit:         500
threshold:     0
```

Optional audit request:

```text
audit lookback: 30 minutes
```

The test used a 30-minute audit window periodically to verify that the 5-minute live window did not miss delayed items.

If:

```text
len(items) == 500
```

or:

```text
hasNextPage == true
```

treat the window as incomplete.

Do not conclude that a thesis is absent from an incomplete window.

---

## 7. Rate limiting

The exact Fomo server-side hard limit is unknown.

Verified test configuration:

```text
5 accounts
1 global request every 2 seconds
≈ 30 requests/min total
≈ 6 requests/min/account
```

Observed during the 3.5-hour test:

```text
401 = 0
403 = 0
429 = 0
```

A conservative per-account starting limit:

```go
PerAccountMinInterval = 1500 * time.Millisecond
```

This is a client-side safety setting, not a confirmed official Fomo limit.

Production rate should remain configurable and should be increased only after controlled load testing.

---

## 8. Scheduler

Use a token task queue and an account pool.

Flow:

```text
token is due
    ↓
Scheduler
    ↓
AccountPool.Acquire()
    ↓
choose HEALTHY account
whose cooldown/rate-limit interval has expired
    ↓
FetchSortedThesis()
```

Suggested account state:

```go
type Account struct {
    ID string

    DebugURL string
    Profile  string

    AccessToken string
    ExpiresAt   time.Time

    State AccountState

    LastRequestAt time.Time
    LastSuccessAt time.Time

    CooldownUntil time.Time

    Consecutive401 int
    Consecutive403 int
    Consecutive429 int
}
```

Recommended states:

```text
HEALTHY
REFRESHING
AUTH_COOLDOWN
RATE_LIMITED
UNHEALTHY
```

---

## 9. Deduplication

Do not rely only on Fomo `thesis_id`.

During testing, logically identical thesis entries could have different IDs depending on the Fomo account that returned them.

Use a global canonical key, for example:

```text
SHA256(
    networkID +
    tokenAddress +
    createdAt +
    userID +
    comment
)
```

Still preserve the original fields:

```text
thesis_id
comment_id
trade_id
user_id
```

Dedup must be shared across **all accounts**.

---

## 10. Baseline and new thesis detection

For every tracked token keep:

```text
trackedAt
```

Initial historical data before tracking is baseline.

A thesis is considered new when:

```text
createdAt >= trackedAt
AND
canonicalID has not been seen before
```

Emit an event like:

```go
type NewThesisEvent struct {
    TokenAddress string
    NetworkID    int64

    ThesisID string
    UserID   string
    Author   string
    Comment  string

    CreatedAt  time.Time
    ReceivedAt time.Time
}
```

Typical downstream flow:

```text
NEW_THESIS
   ↓
database
   ↓
backend WebSocket
   ↓
frontend
   ↓
async translation / alerts / thesis momentum
```

Translation should not block thesis detection.

---

## 11. Error handling

### Success

```text
HTTP 200
AND body.statusCode == 200
→ success
```

### 401

```text
HTTP 401
or body.statusCode == 401
→ invalidate cached access token
→ recover browser-owned auth
→ retry once
```

### 403

```text
HTTP 403
or body.statusCode == 403
→ browser-session recovery
→ retry once
→ if still failing, auth cooldown
```

### 429

```text
→ respect Retry-After when available
→ otherwise use cooldown/backoff
```

Suggested fallback:

```text
5m → 10m → 20m → 30m
```

### 430 / 431

If the body says unauthorized, classify as an auth failure.

### Network failures

Examples:

```text
Failed to fetch
timeout
connection reset
temporary loss of Internet
```

These are **not auth failures**.

Do not place the account into auth cooldown.

Return/retry the job through the scheduler.

### Context cancellation

```text
context canceled
context deadline exceeded
```

These must not automatically be treated as auth errors.

---

## 12. Response parsing

Do not assume that `responseObject` always has the success shape.

Success example:

```json
{
  "success": true,
  "responseObject": {
    "items": [],
    "hasNextPage": false,
    "count": 123
  },
  "statusCode": 200
}
```

Error responses can have a different structure, for example:

```json
{
  "success": false,
  "message": "Authentication token is invalid",
  "responseObject": [],
  "statusCode": 401
}
```

Parse the envelope first:

```text
success
message
statusCode
responseObject as raw JSON
```

Only decode `responseObject.items` after confirming success.

---

## 13. Logging

Every poll should log:

```text
account
tokenAddress
networkID

scheduledAt
dispatchedAt
requestStartedAt

browserFetchStartedAt
browserHeadersReceivedAt
browserBodyReadAt
goReceivedAt

HTTP status
body status

items
response count
hasNextPage
limit hit

latency
query window
query mode
```

For every new thesis log:

```text
createdAt backend
receivedAt
detection delay

token address
network ID

canonical ID
thesis ID
comment ID
trade ID
user ID

author
comment

account that detected it
```

Never log access/refresh tokens.

---

## 14. Tested result

The final test used:

```text
5 accounts
25 tokens
3.5 hours
/feed/token/sortedThesis
5-minute live window
30-minute periodic audit
limit=500
threshold=0
```

Observed:

```text
6285 total polls
6283 successful
2 network failures

401 = 0
403 = 0
429 = 0

2283 unique new theses
0 saturated windows
```

The token polling cycle in that test was approximately 50 seconds because:

```text
25 tokens × one global request every 2 seconds
```

The Fomo request itself normally took only a few hundred milliseconds.

The new thesis was present in the first successful eligible request after its backend `createdAt` for all observed thesis events in the test.

---

## 15. Production architecture

```text
                    Token Tracker
                         │
                         ▼
                    Scheduler
                         │
                         ▼
                   Account Pool
              ┌──────────┼──────────┐
              ▼          ▼          ▼
           acc01       acc02       acc05
              │
              ▼
       persistent Chrome
              │
         Fomo + Privy
              │
       current access JWT
              │
              ▼
 /feed/token/sortedThesis
  5m / limit=500 / threshold=0
              │
              ▼
       response parser
              │
              ▼
        global dedup
              │
              ▼
         NEW_THESIS
              │
       ┌──────┼──────────┐
       ▼      ▼          ▼
      DB   WebSocket  Translator
```

---

## 16. Production requirements

Before production release:

```text
1. Account refresh must not block the global scheduler.
2. Account pool must skip refreshing/cooldown accounts.
3. Per-account rate limit must be configurable.
4. 401/403/429/network errors must be classified separately.
5. Global canonical thesis dedup is required.
6. Browser/CDP ports must remain localhost-only.
7. Access and refresh tokens must never be logged.
8. Every account needs an independent persistent browser profile.
9. Detect `limit=500` saturation and incomplete windows.
10. Store metrics for request latency, token refresh, errors and thesis detection delay.
```

## Main rule

Do not move back to:

```text
login once
→ copy JWT
→ Go owns Privy refresh
```

The tested stable model is:

```text
persistent real Fomo browser session
→ Fomo/Privy owns auth lifecycle
→ Go observes current access token
→ Go schedules thesis requests
```
