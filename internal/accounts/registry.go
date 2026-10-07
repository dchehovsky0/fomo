package accounts

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

type Registry struct {
	list []*Account
	log  *slog.Logger
	now  func() time.Time

	mu       sync.Mutex
	prev     map[string]State
	demand   func() float64
	watching func() int
	axiom    func() any
}

func NewRegistry(log *slog.Logger) *Registry {
	return &Registry{log: log, now: time.Now, prev: map[string]State{}}
}

// Add registers an account; call it for every account before Run.
func (r *Registry) Add(name, proxy string, rps float64) *Account {
	if rps <= 0 {
		rps = 1
	}
	a := &Account{name: name, proxy: maskProxy(proxy), rps: rps}
	r.list = append(r.list, a)
	return a
}

// SetDemand supplies how many fomo requests per second the tokens in work need.
// The health report compares it with the accounts' rate limits.
func (r *Registry) SetDemand(fn func() float64) {
	r.mu.Lock()
	r.demand = fn
	r.mu.Unlock()
}

// SetWatching supplies how many tokens are in the fomo checks right now.
func (r *Registry) SetWatching(fn func() int) {
	r.mu.Lock()
	r.watching = fn
	r.mu.Unlock()
}

// SetAxiom attaches the cap screen's report. The health handler includes it
// as "axiom" next to the accounts.
func (r *Registry) SetAxiom(fn func() any) {
	r.mu.Lock()
	r.axiom = fn
	r.mu.Unlock()
}

type Report struct {
	CheckedAt time.Time `json:"checked_at"`
	Total     int       `json:"total"`
	Healthy   int       `json:"healthy"`
	// Watching is how many tokens are in the fomo checks at this moment.
	Watching int `json:"watching"`
	// Unhealthy lists the accounts that need attention, by name.
	Unhealthy []string `json:"unhealthy,omitempty"`
	// Capacity says whether the working accounts' rate limit covers the
	// tokens currently being watched.
	Capacity Capacity `json:"capacity"`
	Accounts []Status `json:"accounts"`
}

func (r *Registry) Report() Report {
	r.mu.Lock()
	demandFn := r.demand
	watchingFn := r.watching
	r.mu.Unlock()
	var demand float64
	if demandFn != nil {
		demand = demandFn()
	}
	var watching int
	if watchingFn != nil {
		watching = watchingFn()
	}
	now := r.now()
	rep := Report{CheckedAt: now, Total: len(r.list), Watching: watching, Accounts: make([]Status, 0, len(r.list))}
	var configured, working float64
	workingN := 0
	for _, a := range r.list {
		s := a.Status(now)
		rep.Accounts = append(rep.Accounts, s)
		configured += a.rps
		if s.Healthy {
			rep.Healthy++
			working += a.rps
			workingN++
		} else {
			rep.Unhealthy = append(rep.Unhealthy, s.Name)
		}
	}
	rep.Capacity = newCapacity(demand, configured, working, len(r.list), workingN)
	return rep
}

// probeTimeout covers a Chrome launch for a fresh token (page_load_timeout).
const probeTimeout = 90 * time.Second

// Probe checks the given accounts at once and waits for them.
func (r *Registry) Probe(ctx context.Context, list []*Account) {
	var wg sync.WaitGroup
	for _, a := range list {
		if a.probe == nil {
			continue
		}
		wg.Go(func() {
			pctx, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()
			started := time.Now()
			err := a.runProbe(pctx)
			took := time.Since(started).Round(time.Millisecond)
			if err != nil && ctx.Err() == nil {
				r.log.Warn("account probe failed", "account", a.name, "took", took, "err", err)
				return
			}
			if ctx.Err() == nil {
				r.log.Info("account probe", "account", a.name, "took", took)
			}
		})
	}
	wg.Wait()
}

// Run probes accounts that had no successful request for idleAfter and logs
// every change of an account's state, until ctx ends.
func (r *Registry) Run(ctx context.Context, every, idleAfter time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		now := r.now()
		var idle []*Account
		for _, a := range r.list {
			if a.idle(now, idleAfter) {
				idle = append(idle, a)
			}
		}
		r.Probe(ctx, idle)
		if ctx.Err() != nil {
			return
		}
		r.logChanges()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (r *Registry) logChanges() {
	rep := r.Report()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range rep.Accounts {
		prev, seen := r.prev[s.Name]
		r.prev[s.Name] = s.State
		if s.State == prev || (!seen && s.State == StateUnknown) {
			continue
		}
		if s.Healthy {
			r.log.Info("fomo account works", "account", s.Name, "state", s.State)
		} else {
			r.log.Warn("fomo account does not work", "account", s.Name, "state", s.State, "reason", s.Reason, "last_error", s.LastError)
		}
	}
}

// Handler serves the report as JSON: 200 when every account works, 503
// otherwise. ?probe=1 checks all accounts first instead of using what the
// bot saw in the last minutes.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Query().Get("probe") == "1" {
			r.Probe(req.Context(), r.list)
			r.logChanges()
		}
		rep := r.Report()
		h := w.Header()
		h.Set("Content-Type", "application/json; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		if rep.Healthy < rep.Total || rep.Total == 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		r.mu.Lock()
		ax := r.axiom
		r.mu.Unlock()
		body := struct {
			Report
			Axiom any `json:"axiom,omitempty"`
		}{Report: rep}
		if ax != nil {
			body.Axiom = ax()
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		_ = enc.Encode(body)
	})
}
