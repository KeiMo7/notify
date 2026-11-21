package reliability

import (
	"fmt"
	"sync"
	"time"

	"github.com/pkg/errors"
)

// Sender is the subset of providers.Provider the reliability layer needs.
// It matches the existing interface exactly so every existing adapter can be
// wrapped without modification.
type Sender interface {
	Send(message, cliFormat string) error
}

// ErrCircuitOpen is returned (wrapped) when a degraded provider is skipped
// without any network attempt. It carries no request, so skipping never
// amplifies load on the struggling channel.
var ErrCircuitOpen = errors.New("provider degraded (circuit open), send skipped")

// signaledFailure synthesizes an error when the adapter itself returned nil
// but the instrumented transport observed an explicit channel failure status
// (e.g. a custom webhook that does not check status codes). Its text still
// carries the class keywords so the error-text fallback stays consistent.
type signaledFailure struct {
	class      FailureClass
	retryAfter time.Duration
}

func (e *signaledFailure) Error() string {
	return fmt.Sprintf("channel signaled failure: %s (retry_after=%s)", e.class, e.retryAfter)
}

// SendError is the structured outcome of a failed reliable send. It carries
// the classified cause so callers can distinguish "queue this" from
// "alert and drop".
type SendError struct {
	Provider string
	Class    FailureClass
	Attempts int // total attempts made (1 == original only, no retry happened)
	LastErr  error
	Open     bool // true when the breaker tripped open as a result of this send
}

func (e *SendError) Error() string {
	return fmt.Sprintf("reliability: provider %q failed after %d attempt(s), class=%s: %v",
		e.Provider, e.Attempts, e.Class, e.LastErr)
}

// Unwrap exposes the underlying adapter error for errors.Is/As.
func (e *SendError) Unwrap() error { return e.LastErr }

// Counters aggregates what happened to one provider since startup.
type Counters struct {
	Attempts       int64
	Retries        int64
	RateLimited    int64
	AuthFailed     int64
	ContentReject  int64
	Unreachable    int64
	UnknownFailure int64
	OpenSkipped    int64
}

// ReliableProvider wraps an existing Sender with classification, bounded
// exponential backoff and a circuit breaker. The wrapped adapter and its
// configuration are untouched.
type ReliableProvider struct {
	name      string
	delegate  Sender
	cfg       Config
	breaker   *Breaker
	collector *signalCollector

	mu       sync.Mutex
	counters Counters

	// Indirections exist so tests can drive time deterministically;
	// production wiring uses the real clock.
	now   func() time.Time
	sleep func(time.Duration)
}

// NewReliableProvider wraps delegate. cfg must already be defaulted+validated.
func NewReliableProvider(name string, delegate Sender, cfg Config, collector *signalCollector) *ReliableProvider {
	return &ReliableProvider{
		name:      name,
		delegate:  delegate,
		cfg:       cfg,
		breaker:   NewBreaker(cfg),
		collector: collector,
		now:       time.Now,
		sleep:     time.Sleep,
	}
}

// Name identifies the provider in logs and the status snapshot.
func (p *ReliableProvider) Name() string { return p.name }

// Breaker exposes the breaker for status reporting.
func (p *ReliableProvider) Breaker() *Breaker { return p.breaker }

// Counters returns a copy of the provider counters.
func (p *ReliableProvider) Counters() Counters {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.counters
}

func (p *ReliableProvider) count(class FailureClass) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.counters.Attempts++
	switch class {
	case ClassRateLimited:
		p.counters.RateLimited++
	case ClassAuthFailed:
		p.counters.AuthFailed++
	case ClassContentRejected:
		p.counters.ContentReject++
	case ClassUnreachable:
		p.counters.Unreachable++
	default:
		p.counters.UnknownFailure++
	}
}

// backoff computes the delay before attempt number `retry` (0-based: retry 0
// is the first re-attempt). The channel's explicit Retry-After is honored by
// taking the larger value, then the whole delay is checked against the
// remaining retry budget: if it does not fit, the second return is false and
// the caller must stop rather than overrun the total-push bound.
func (p *ReliableProvider) backoff(retry int, retryAfter time.Duration, deadline time.Time, now time.Time) (time.Duration, bool) {
	d := p.cfg.BackoffInitial
	// Shift only while it stays safe; once 2^retry would overflow or exceed
	// the configured cap, clamp to BackoffMax instead of producing a negative
	// (or wrapped) delay.
	if retry > 0 && retry < 63 {
		shifted := p.cfg.BackoffInitial << retry
		if shifted > 0 && shifted <= p.cfg.BackoffMax {
			d = shifted
		} else {
			d = p.cfg.BackoffMax
		}
	} else if retry >= 63 {
		d = p.cfg.BackoffMax
	}
	if d <= 0 || d > p.cfg.BackoffMax {
		d = p.cfg.BackoffMax
	}
	if retryAfter > d {
		d = retryAfter
	}
	remaining := deadline.Sub(now)
	if remaining <= 0 || d > remaining {
		return 0, false
	}
	return d, true
}

// Send implements the same contract as the underlying adapter but applies
// the reliability policy. On success the return is exactly nil; on failure a
// *SendError is returned, never a raw adapter error.
func (p *ReliableProvider) Send(message, cliFormat string) error {
	now := p.now()
	if !p.breaker.Allow(now) {
		p.mu.Lock()
		p.counters.OpenSkipped++
		p.mu.Unlock()
		return &SendError{
			Provider: p.name,
			Class:    ClassUnreachable,
			Attempts: 0,
			LastErr:  errors.Wrap(ErrCircuitOpen, p.name),
		}
	}

	deadline := p.now().Add(p.cfg.MaxRetryTime)
	attempts := 0

	var (
		lastClass  FailureClass
		lastErr    error
		retryAfter time.Duration
	)

	for {
		attempts++
		// Open the observation window around the adapter call so every HTTP
		// round-trip it performs (including redirects) is attributed here.
		p.collector.Begin()
		err := p.delegate.Send(message, cliFormat)
		records := p.collector.End()

		// Some adapters (notably the custom webhook) do not inspect the HTTP
		// status code and return nil even for a 4xx/5xx. The instrumented
		// transport still saw the channel's explicit answer, so treat a
		// failure-class record as authoritative regardless of the adapter's
		// nil error. A transport error likewise surfaces here.
		signaledClass, signaledRetry := Classify(records, err)
		if err == nil && signaledClass != ClassOK && signaledClass != ClassUnknown {
			err = &signaledFailure{class: signaledClass, retryAfter: signaledRetry}
		}

		if err == nil {
			p.mu.Lock()
			p.counters.Attempts++
			p.mu.Unlock()
			p.breaker.OnSuccess()
			return nil
		}

		// Assign (not :=): the result must outlive the loop iteration and
		// reach the SendError returned below. A short declaration here would
		// shadow the outer lastClass and silently leave it as ClassOK.
		lastClass, retryAfter = Classify(records, err)
		p.count(lastClass)
		lastErr = err

		// Deterministic failures never retry: an auth failure or a rejected
		// payload would produce the identical answer again.
		if !lastClass.Retryable() {
			break
		}
		// Hard cap on attempt count: retries must never amplify request
		// volume beyond 1 + MaxRetries per send.
		if attempts-1 >= p.cfg.MaxRetries {
			break
		}

		// The deadline is fixed once at entry; recomputing it against a
		// clock that only advances inside the injected sleep would make the
		// budget look perpetually unspent (or perpetually expired).
		delay, fits := p.backoff(attempts-1, retryAfter, deadline, p.now())
		if !fits {
			break
		}

		p.mu.Lock()
		p.counters.Retries++
		p.mu.Unlock()
		p.sleep(delay)
	}

	opened := p.breaker.OnFailure(p.now())
	return &SendError{
		Provider: p.name,
		Class:    lastClass,
		Attempts: attempts,
		LastErr:  lastErr,
		Open:     opened,
	}
}
