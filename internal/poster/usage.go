package poster

import (
	"errors"
	"sync"
)

// DailyRequestLimit is the free OMDb tier's daily allowance. The API reports
// nothing about remaining quota — no header, no field — so the only way to show
// it is to count what we send. The count starts fresh every time the app starts.
const DailyRequestLimit = 1000

// ErrQuotaExceeded is returned once the backend says the day's allowance is
// gone, so callers can say that instead of "something went wrong".
var ErrQuotaExceeded = errors.New("daily API request limit reached")

// Usage is a snapshot of what this run has spent.
type Usage struct {
	Source    string `json:"source"` // "IMDb", "TMDB" or "" when lookups are off
	Used      int    `json:"used"`
	Limit     int    `json:"limit"`
	Remaining int    `json:"remaining"`
	Percent   int    `json:"percent"`   // of the limit, for the meter bar
	Low       bool   `json:"low"`       // under a tenth left
	Exhausted bool   `json:"exhausted"` // the API itself told us we are out
}

// meter counts requests actually sent to the backend. Cache hits never reach a
// provider, so they never count here — which is the point of the cache.
type meter struct {
	mu        sync.Mutex
	used      int
	exhausted bool
}

func (m *meter) record() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.used++
	m.mu.Unlock()
}

// markExhausted records that the backend refused us for quota reasons.
func (m *meter) markExhausted() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.exhausted = true
	m.mu.Unlock()
}

func (m *meter) snapshot(source string, limit int) Usage {
	if m == nil {
		return Usage{}
	}
	m.mu.Lock()
	used, exhausted := m.used, m.exhausted
	m.mu.Unlock()

	if limit <= 0 {
		limit = DailyRequestLimit
	}
	remaining := limit - used
	if remaining < 0 || exhausted {
		remaining = 0
	}
	percent := used * 100 / limit
	if percent > 100 {
		percent = 100
	}
	return Usage{
		Source:    source,
		Used:      used,
		Limit:     limit,
		Remaining: remaining,
		Percent:   percent,
		Low:       remaining <= limit/10,
		Exhausted: exhausted || remaining == 0,
	}
}
