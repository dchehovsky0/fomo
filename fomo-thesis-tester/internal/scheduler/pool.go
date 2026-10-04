package scheduler

import (
	"errors"
	"sync"
	"time"
)

type Pool struct {
	Accounts []*Account
	mu       sync.Mutex
	next     int
}

// Pick is deterministic round-robin across healthy/available accounts. This
// removes random account skew from the stability experiment while still
// skipping accounts that are cooling down or rate-limited.
func (p *Pool) Pick(now time.Time) (*Account, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.Accounts) == 0 {
		return nil, errors.New("empty account pool")
	}
	for offset := 0; offset < len(p.Accounts); offset++ {
		i := (p.next + offset) % len(p.Accounts)
		if p.Accounts[i].Available(now) {
			p.next = (i + 1) % len(p.Accounts)
			return p.Accounts[i], nil
		}
	}
	return nil, errors.New("no account currently available")
}
