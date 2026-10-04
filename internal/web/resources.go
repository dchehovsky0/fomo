package web

import (
	"encoding/json"
	"net/http"
	"runtime"
	"runtime/metrics"
	"sync"
	"time"
)

// Resources reports what this process is using: memory, goroutines and CPU.
// cpu_percent is the average since the process started and can pass 100 when
// several cores are busy. cpu_percent_recent is the same measure since the
// previous request to this handler.
func Resources() http.Handler {
	started := time.Now()
	var mu sync.Mutex
	var prevCPU float64
	var prevAt time.Time
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		now := time.Now()
		cpu, haveCPU := processCPUSeconds()
		body := resourceBody{
			At:           now,
			UptimeSec:    now.Sub(started).Seconds(),
			Goroutines:   runtime.NumGoroutine(),
			NumCPU:       runtime.NumCPU(),
			HeapAlloc:    ms.HeapAlloc,
			HeapInuse:    ms.HeapInuse,
			HeapSys:      ms.HeapSys,
			Sys:          ms.Sys,
			NumGC:        ms.NumGC,
			GCPauseTotal: time.Duration(ms.PauseTotalNs).Seconds() * 1000,
		}
		if haveCPU {
			body.CPUSeconds = &cpu
			pct := 0.0
			if up := body.UptimeSec; up > 0 {
				pct = cpu / up * 100
			}
			body.CPUPercent = &pct
			mu.Lock()
			if !prevAt.IsZero() {
				if dt := now.Sub(prevAt).Seconds(); dt > 0 {
					recent := (cpu - prevCPU) / dt * 100
					body.CPUPercentRecent = &recent
				}
			}
			prevCPU, prevAt = cpu, now
			mu.Unlock()
		}
		h := w.Header()
		h.Set("Content-Type", "application/json; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(body)
	})
}

type resourceBody struct {
	At               time.Time `json:"at"`
	UptimeSec        float64   `json:"uptime_sec"`
	Goroutines       int       `json:"goroutines"`
	NumCPU           int       `json:"num_cpu"`
	HeapAlloc        uint64    `json:"heap_alloc"`
	HeapInuse        uint64    `json:"heap_inuse"`
	HeapSys          uint64    `json:"heap_sys"`
	Sys              uint64    `json:"sys"`
	NumGC            uint32    `json:"num_gc"`
	GCPauseTotal     float64   `json:"gc_pause_total_ms"`
	CPUSeconds       *float64  `json:"cpu_seconds,omitempty"`
	CPUPercent       *float64  `json:"cpu_percent,omitempty"`
	CPUPercentRecent *float64  `json:"cpu_percent_recent,omitempty"`
}

func processCPUSeconds() (float64, bool) {
	sample := []metrics.Sample{{Name: "/cpu/classes/total:cpu-seconds"}}
	metrics.Read(sample)
	if sample[0].Value.Kind() != metrics.KindFloat64 {
		return 0, false
	}
	return sample[0].Value.Float64(), true
}
