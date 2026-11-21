package reliability

import (
	"sync"
	"time"
)

// BreakerState describes the circuit state of one provider.
type BreakerState int

const (
	// StateClosed: traffic flows normally.
	StateClosed BreakerState = iota
	// StateOpen: provider is paused (degraded), sends are short-circuited.
	StateOpen
	// StateHalfOpen: a single probe has been allowed to test recovery.
	StateHalfOpen
)

// String renders the state for the status snapshot and logs.
func (s BreakerState) String() string {
	switch s {
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half_open"
	default:
		return "closed"
	}
}

// Breaker is a per-provider circuit breaker. It is the only place that
// decides "this channel is degraded": the surrounding retry logic handles
// individual attempt failures, the breaker handles sustained failure.
type Breaker struct {
	cfg Config

	mu               sync.Mutex
	state            BreakerState
	consecutiveFails int
	totalFails       int64
	openedAt         time.Time
	halfOpenInFlight bool
}

// NewBreaker builds a breaker from an already-defaulted, validated Config.
func NewBreaker(cfg Config) *Breaker {
	return &Breaker{cfg: cfg, state: StateClosed}
}

// Allow decides whether a send may proceed now. When the circuit is open and
// the cooldown has elapsed, exactly one caller is admitted into half-open
// state; everyone else is rejected until the probe resolves.
func (b *Breaker) Allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateClosed:
		return true
	case StateHalfOpen:
		return b.halfOpenInFlight == false
	default: // StateOpen
		if now.Sub(b.openedAt) >= b.cfg.Cooldown {
			b.state = StateHalfOpen
			b.halfOpenInFlight = true
			return true
		}
		return false
	}
}

// OnSuccess closes the circuit and resets the consecutive-failure streak.
// A successful half-open probe therefore restores the provider immediately.
func (b *Breaker) OnSuccess() {
	b.mu.Lock()
	b.state = StateClosed
	b.consecutiveFails = 0
	b.halfOpenInFlight = false
	b.mu.Unlock()
}

// OnFailure records a failed send. In half-open state any failure re-opens
// the circuit immediately; in closed state the circuit opens once the
// configured consecutive-failure threshold is reached.
func (b *Breaker) OnFailure(now time.Time) (opened bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.totalFails++
	b.halfOpenInFlight = false

	if b.state == StateHalfOpen {
		b.state = StateOpen
		b.openedAt = now
		return true
	}

	b.consecutiveFails++
	if b.cfg.FailureThreshold > 0 && b.consecutiveFails >= b.cfg.FailureThreshold && b.state == StateClosed {
		b.state = StateOpen
		b.openedAt = now
		return true
	}
	return false
}

// Snapshot is an immutable view of the breaker for observability.
type Snapshot struct {
	State            BreakerState
	ConsecutiveFails int
	TotalFails       int64
	OpenedAt         time.Time
}

// Status returns a point-in-time snapshot without exposing the lock.
func (b *Breaker) Status() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Snapshot{
		State:            b.state,
		ConsecutiveFails: b.consecutiveFails,
		TotalFails:       b.totalFails,
		OpenedAt:         b.openedAt,
	}
}
