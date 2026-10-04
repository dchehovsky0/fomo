package accounts

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"fomobot/internal/fomo"
)

func TestCapacity(t *testing.T) {
	r := NewRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)))
	a := r.Add("main", "", 1)
	b := r.Add("acc2", "", 1)
	r.Add("acc3", "", 1)
	a.ObserveCall(fomo.CallInfo{At: time.Now(), Status: 200})
	b.ObserveCall(fomo.CallInfo{At: time.Now(), Status: 200})

	r.SetDemand(func() float64 { return 0 })
	idle := r.Report().Capacity
	if !idle.Enough || idle.AccountsNeeded != 0 || !strings.Contains(idle.Summary, "нечего") {
		t.Fatalf("idle: %+v", idle)
	}

	r.SetDemand(func() float64 { return 1.2 })
	ok := r.Report().Capacity
	if !ok.Enough || ok.WorkingAccounts != 2 || ok.WorkingRPS != 2 || ok.CapacityRPS != 3 || ok.AccountsNeeded != 2 || ok.AccountsShort != 0 {
		t.Fatalf("enough: %+v", ok)
	}
	if !strings.Contains(ok.Summary, "Хватает") {
		t.Fatalf("summary: %s", ok.Summary)
	}

	r.SetDemand(func() float64 { return 2.4 })
	shortWorkers := r.Report().Capacity
	if shortWorkers.Enough || shortWorkers.AccountsShort != 1 || shortWorkers.RPSShort != 0.4 || !strings.Contains(shortWorkers.Summary, "Рабочих аккаунтов не хватает") {
		t.Fatalf("workers short: %+v", shortWorkers)
	}

	r.SetDemand(func() float64 { return 5 })
	shortLimit := r.Report().Capacity
	if shortLimit.Enough || shortLimit.AccountsNeeded != 5 || shortLimit.AccountsShort != 3 || shortLimit.RPSShort != 3 || !strings.Contains(shortLimit.Summary, "Не хватает лимита") {
		t.Fatalf("limit short: %+v", shortLimit)
	}
}
