package reliability

import (
	"sync"
	"time"
)

// Deduper coalesces identical messages inside a rolling time window: the
// first occurrence is delivered immediately, repeated occurrences inside the
// window are counted, and a single summary is delivered when the window
// closes (lazily, on the next Admit call — there is no background goroutine
// and therefore no extra synchronization point or shutdown race).
type Deduper struct {
	window time.Duration
	now    func() time.Time

	mu      sync.Mutex
	pending map[string]*pendingEntry
}

type pendingEntry struct {
	firstSeen time.Time
	count     int
}

// NewDeduper builds a deduper. A zero window disables coalescing: Admit then
// always reports a fresh message.
func NewDeduper(window time.Duration) *Deduper {
	return &Deduper{
		window:  window,
		now:     time.Now,
		pending: make(map[string]*pendingEntry),
	}
}

// AdmitDecision tells the caller how to handle a message.
type AdmitDecision struct {
	// Fresh is true when the message must be sent now.
	Fresh bool
	// Merged is the number of duplicates swallowed since the fresh send.
	// It is zero when Fresh is true.
	Merged int
	// DueSummaries are windows that closed since the previous call. Each is
	// delivered once, as a single summary message, then removed.
	DueSummaries []Summary
}

// Summary is the coalesced replacement for suppressed duplicates.
type Summary struct {
	Message string
	Count   int // total occurrences inside the window (>= 2)
}

// Admit registers a message and returns what to do with it.
func (d *Deduper) Admit(message string) AdmitDecision {
	if d.window <= 0 {
		return AdmitDecision{Fresh: true}
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	now := d.now()
	decision := AdmitDecision{}

	// Flush any windows that have elapsed before evaluating the new message.
	for key, entry := range d.pending {
		if now.Sub(entry.firstSeen) >= d.window {
			if entry.count > 1 {
				decision.DueSummaries = append(decision.DueSummaries, Summary{
					Message: key,
					Count:   entry.count,
				})
			}
			delete(d.pending, key)
		}
	}

	if entry, ok := d.pending[message]; ok {
		entry.count++
		decision.Merged = entry.count - 1
		decision.Fresh = false
		return decision
	}

	d.pending[message] = &pendingEntry{firstSeen: now, count: 1}
	decision.Fresh = true
	return decision
}

// Flush forces emission of all still-open windows. It is used at shutdown so
// summaries are not lost; entries with a single occurrence produce nothing
// because that message was already delivered when it was first admitted.
func (d *Deduper) Flush() []Summary {
	if d.window <= 0 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	out := make([]Summary, 0, len(d.pending))
	for key, entry := range d.pending {
		if entry.count > 1 {
			out = append(out, Summary{Message: key, Count: entry.count})
		}
		delete(d.pending, key)
	}
	return out
}
