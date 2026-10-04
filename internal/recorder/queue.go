package recorder

import (
	"context"
	"sync"
	"time"
)

type jobKind int

const (
	jobHistory jobKind = iota // first look at a token: thesis pages up to job.pages
	jobRefresh                // first thesis page: count and newest theses
	jobDetails                // /proxy/tokenDetails
)

func (k jobKind) String() string {
	switch k {
	case jobHistory:
		return "history"
	case jobRefresh:
		return "refresh"
	default:
		return "details"
	}
}

const (
	prioUrgent   = iota // live activity: a new thesis, a young token
	prioBackfill        // tokens that existed before, periodic rechecks
	prioLow             // token details
)

type job struct {
	kind    jobKind
	mint    string
	prio    int
	pages   int
	due     time.Time
	reason  string
	attempt int
}

func (j job) kindName() string { return j.kind.String() }

type jobKey struct {
	kind jobKind
	mint string
}

// queue holds at most one job per (kind, token). All API calls share one rate
// limit, so the order matters: due jobs run by priority, then by due time.
type queue struct {
	mu     sync.Mutex
	jobs   map[jobKey]job
	wakeup chan struct{}
}

func newQueue() *queue {
	return &queue{jobs: map[jobKey]job{}, wakeup: make(chan struct{}, 1)}
}

// push adds a job or merges it into the pending one: the earlier due time,
// the higher priority and the larger page budget win.
func (q *queue) push(j job) {
	q.mu.Lock()
	k := jobKey{j.kind, j.mint}
	if cur, ok := q.jobs[k]; ok {
		if cur.due.Before(j.due) {
			j.due = cur.due
		}
		j.prio = min(j.prio, cur.prio)
		j.pages = max(j.pages, cur.pages)
		j.attempt = min(j.attempt, cur.attempt)
		if j.reason == "" {
			j.reason = cur.reason
		}
	}
	q.jobs[k] = j
	q.mu.Unlock()
	select {
	case q.wakeup <- struct{}{}:
	default:
	}
}

func (q *queue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.jobs)
}

// pop blocks until a job is due or ctx is done.
func (q *queue) pop(ctx context.Context, now func() time.Time) (job, bool) {
	for {
		q.mu.Lock()
		t := now()
		var best job
		var found bool
		var next time.Time
		for _, j := range q.jobs {
			if j.due.After(t) {
				if next.IsZero() || j.due.Before(next) {
					next = j.due
				}
				continue
			}
			if !found || j.prio < best.prio || j.prio == best.prio && j.due.Before(best.due) {
				best, found = j, true
			}
		}
		if found {
			delete(q.jobs, jobKey{best.kind, best.mint})
		}
		q.mu.Unlock()
		if found {
			return best, true
		}

		wait := time.Second
		if !next.IsZero() {
			wait = min(max(next.Sub(t), 10*time.Millisecond), time.Second)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return job{}, false
		case <-q.wakeup:
		case <-timer.C:
		}
		timer.Stop()
	}
}
