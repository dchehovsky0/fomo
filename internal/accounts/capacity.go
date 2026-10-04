package accounts

import (
	"fmt"
	"math"
	"strconv"
)

// Capacity compares the request rate the watched tokens need with what the
// accounts can actually send.
type Capacity struct {
	// DemandRPS is how many fomo requests per second the current tokens need.
	DemandRPS float64 `json:"demand_rps"`
	// CapacityRPS is the configured limit of every account, working or not.
	CapacityRPS float64 `json:"capacity_rps"`
	// WorkingRPS is the limit of accounts that are healthy right now.
	WorkingRPS      float64 `json:"working_rps"`
	Accounts        int     `json:"accounts"`
	WorkingAccounts int     `json:"working_accounts"`
	// AccountsNeeded is how many accounts at the configured rate cover the demand.
	AccountsNeeded int `json:"accounts_needed"`
	// AccountsShort is how many more working accounts are missing.
	AccountsShort int     `json:"accounts_short"`
	RPSShort      float64 `json:"rps_short"`
	Enough        bool    `json:"enough"`
	Summary       string  `json:"summary"`
}

func newCapacity(demand, configured, working float64, accounts, workingN int) Capacity {
	avg := 1.0
	if accounts > 0 && configured > 0 {
		avg = configured / float64(accounts)
	}
	needed := 0
	if demand > 0 {
		needed = int(math.Ceil(demand / avg))
	}
	shortN := needed - workingN
	if shortN < 0 {
		shortN = 0
	}
	shortRPS := demand - working
	if shortRPS < 0 {
		shortRPS = 0
	}
	c := Capacity{
		DemandRPS:       round1(demand),
		CapacityRPS:     round1(configured),
		WorkingRPS:      round1(working),
		Accounts:        accounts,
		WorkingAccounts: workingN,
		AccountsNeeded:  needed,
		AccountsShort:   shortN,
		RPSShort:        round1(shortRPS),
		Enough:          demand <= working+1e-9,
	}
	c.Summary = c.summary(avg)
	return c
}

func (c Capacity) summary(avg float64) string {
	switch {
	case c.DemandRPS == 0:
		return fmt.Sprintf("Проверять сейчас нечего. В конфиге %d аккаунтов на %s запр/с, работают %d на %s запр/с.",
			c.Accounts, rps(c.CapacityRPS), c.WorkingAccounts, rps(c.WorkingRPS))
	case c.Enough:
		return fmt.Sprintf("Хватает. Спрос %s запр/с, рабочие аккаунты дают %s запр/с (%d из %d).",
			rps(c.DemandRPS), rps(c.WorkingRPS), c.WorkingAccounts, c.Accounts)
	case c.CapacityRPS+1e-9 >= c.DemandRPS:
		return fmt.Sprintf("Рабочих аккаунтов не хватает. Спрос %s запр/с, сейчас %s запр/с (%d из %d). Нужно ещё %d аккаунтов по %s запр/с. Лимит в конфиге %s запр/с — его хватает, если заработают остальные.",
			rps(c.DemandRPS), rps(c.WorkingRPS), c.WorkingAccounts, c.Accounts, c.AccountsShort, rps(round1(avg)), rps(c.CapacityRPS))
	default:
		return fmt.Sprintf("Не хватает лимита. Спрос %s запр/с, в конфиге %s запр/с на %d аккаунтов, работают %d на %s запр/с. Нужно аккаунтов %d (ещё %d) или +%s запр/с.",
			rps(c.DemandRPS), rps(c.CapacityRPS), c.Accounts, c.WorkingAccounts, rps(c.WorkingRPS), c.AccountsNeeded, c.AccountsShort, rps(c.RPSShort))
	}
}

func round1(v float64) float64 {
	return math.Round(v*10) / 10
}

func rps(v float64) string {
	return strconv.FormatFloat(v, 'f', 1, 64)
}
