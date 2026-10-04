package state

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"fomo-thesis-tester/internal/fomo"
)

type PollMeta struct {
	AccountID              string
	Sequence               int64
	TokenPollNumber        int64
	QueryMode              string
	RequestStarted         time.Time
	BrowserHeadersReceived time.Time
	BrowserBodyRead        time.Time
	ReceivedAt             time.Time
	QueryAfter             time.Time
	QueryBefore            time.Time
	ServerDate             string
	Complete               bool
	HasNextPage            bool
	ItemCount              int
	Limit                  int
}

type Alert struct {
	Thesis fomo.Thesis

	CanonicalID string
	CreatedAt   time.Time

	FirstSeenRequestStartedAt  time.Time
	FirstSeenBrowserHeadersAt  time.Time
	FirstSeenBrowserBodyReadAt time.Time
	FirstSeenReceivedAt        time.Time
	DetectionDelay             time.Duration
	BackendReturnedUpperBound  time.Duration
	FetchDuration              time.Duration

	FirstEligiblePollStartedAt  time.Time
	FirstEligiblePollReceivedAt time.Time
	FirstPollWait               time.Duration
	EligiblePollsBeforeSeen     int

	LastAbsentPollReceivedAt time.Time
	BackendLagLowerBound     time.Duration
	BackendLagUpperBound     time.Duration
	BackendVisibilityWindow  time.Duration
	HasAvailabilityBound     bool

	CurrentWindowComplete bool
	LatencyConfidence     string
}

type tokenState struct {
	Initialized bool
	Seen        map[string]struct{}
	History     []PollMeta
}

type Tracker struct {
	mu         sync.Mutex
	tokens     map[string]*tokenState
	startedAt  time.Time
	historyCap int
}

func NewTracker(startedAt time.Time) *Tracker {
	return &Tracker{
		tokens:     make(map[string]*tokenState),
		startedAt:  startedAt.UTC(),
		historyCap: 64,
	}
}

func tokenKey(address string, networkID int64) string {
	return address + "|" + strconv.FormatInt(networkID, 10)
}

// Observe performs account-independent deduplication and derives timing bounds.
// Fomo can expose different thesis IDs for the same logical thesis across accounts,
// so the canonical key intentionally does not rely on Thesis.ID.
func (d *Tracker) Observe(address string, networkID int64, meta PollMeta, items []fomo.Thesis) (alerts []Alert, baseline bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	k := tokenKey(address, networkID)
	st := d.tokens[k]
	if st == nil {
		st = &tokenState{Seen: make(map[string]struct{})}
		d.tokens[k] = st
	}
	baseline = !st.Initialized

	items = append([]fomo.Thesis(nil), items...)
	sortByCreatedAt(items)

	for _, it := range items {
		canonical := CanonicalID(address, networkID, it)
		if canonical == "" {
			continue
		}
		if _, ok := st.Seen[canonical]; ok {
			continue
		}
		st.Seen[canonical] = struct{}{}

		created, err := it.CreatedTime()
		if err != nil {
			continue
		}
		created = created.UTC()
		if created.Before(d.startedAt) {
			continue
		}

		visibleAt := pollVisibilityTime(meta)
		alert := Alert{
			Thesis:                     it,
			CanonicalID:                canonical,
			CreatedAt:                  created,
			FirstSeenRequestStartedAt:  meta.RequestStarted.UTC(),
			FirstSeenBrowserHeadersAt:  meta.BrowserHeadersReceived.UTC(),
			FirstSeenBrowserBodyReadAt: meta.BrowserBodyRead.UTC(),
			FirstSeenReceivedAt:        meta.ReceivedAt.UTC(),
			DetectionDelay:             meta.ReceivedAt.Sub(created),
			BackendReturnedUpperBound:  visibleAt.Sub(created),
			FetchDuration:              visibleAt.Sub(meta.RequestStarted),
			CurrentWindowComplete:      meta.Complete,
			BackendLagUpperBound:       visibleAt.Sub(created),
			LatencyConfidence:          "unbounded",
		}

		eligible := eligiblePolls(st.History, created)
		if len(eligible) > 0 {
			first := eligible[0]
			alert.FirstEligiblePollStartedAt = first.RequestStarted.UTC()
			alert.FirstEligiblePollReceivedAt = pollVisibilityTime(first).UTC()
			alert.FirstPollWait = first.RequestStarted.Sub(created)
			alert.EligiblePollsBeforeSeen = len(eligible)
		} else if covers(meta, created) {
			alert.FirstEligiblePollStartedAt = meta.RequestStarted.UTC()
			alert.FirstEligiblePollReceivedAt = visibleAt.UTC()
			alert.FirstPollWait = meta.RequestStarted.Sub(created)
		}

		// A previous complete window that covered createdAt and still did not expose
		// this canonical thesis gives a real lower bound for backend visibility.
		for i := len(eligible) - 1; i >= 0; i-- {
			p := eligible[i]
			if !p.Complete {
				continue
			}
			lastAbsentAt := pollVisibilityTime(p)
			alert.LastAbsentPollReceivedAt = lastAbsentAt.UTC()
			alert.BackendLagLowerBound = lastAbsentAt.Sub(created)
			alert.BackendVisibilityWindow = visibleAt.Sub(lastAbsentAt)
			alert.HasAvailabilityBound = true
			alert.LatencyConfidence = "bounded_by_complete_polls"
			break
		}
		if !alert.HasAvailabilityBound && len(eligible) == 0 && meta.Complete {
			alert.LatencyConfidence = "seen_on_first_eligible_complete_poll"
		}
		alerts = append(alerts, alert)
	}

	st.Initialized = true
	st.History = append(st.History, meta)
	if len(st.History) > d.historyCap {
		st.History = append([]PollMeta(nil), st.History[len(st.History)-d.historyCap:]...)
	}
	return alerts, baseline
}

func eligiblePolls(history []PollMeta, created time.Time) []PollMeta {
	out := make([]PollMeta, 0, len(history))
	for _, p := range history {
		if covers(p, created) && !pollVisibilityTime(p).Before(created) {
			out = append(out, p)
		}
	}
	return out
}

func covers(p PollMeta, created time.Time) bool {
	return !created.Before(p.QueryAfter) && !created.After(p.QueryBefore)
}

func pollVisibilityTime(p PollMeta) time.Time {
	if !p.BrowserBodyRead.IsZero() {
		return p.BrowserBodyRead
	}
	return p.ReceivedAt
}

func CanonicalID(address string, networkID int64, t fomo.Thesis) string {
	created := strings.TrimSpace(t.CreatedAt)
	if parsed, err := t.CreatedTime(); err == nil {
		created = parsed.UTC().Format(time.RFC3339Nano)
	}
	tokenAddress := strings.TrimSpace(t.TokenAddress)
	if tokenAddress == "" {
		tokenAddress = strings.TrimSpace(address)
	}
	nid := t.NetworkID
	if nid == 0 {
		nid = networkID
	}
	authorIdentity := strings.TrimSpace(t.UserID)
	if authorIdentity == "" {
		authorIdentity = strings.TrimSpace(t.UserHandle)
	}
	identity := strings.Join([]string{
		strconv.FormatInt(nid, 10),
		tokenAddress,
		created,
		authorIdentity,
		strings.TrimSpace(t.Comment.Comment),
	}, "\x1f")
	if strings.Trim(identity, "\x1f") == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}

func sortByCreatedAt(items []fomo.Thesis) {
	sort.SliceStable(items, func(i, j int) bool {
		ti, ei := items[i].CreatedTime()
		tj, ej := items[j].CreatedTime()
		if ei != nil || ej != nil {
			return items[i].CreatedAt < items[j].CreatedAt
		}
		return ti.Before(tj)
	})
}
