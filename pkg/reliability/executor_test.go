package reliability

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeSender is a deterministic adapter: it returns scripted classes by
// recording the equivalent HTTP signal into the collector (the primary
// classification path) and also returning a matching error text (the
// fallback path), and counts every actual invocation so tests can assert
// total request volume.
type fakeSender struct {
	collector *signalCollector
	calls     int32
	script    []scriptedAttempt
}

type scriptedAttempt struct {
	class FailureClass
}

func (f *fakeSender) Send(_, _ string) error {
	idx := int(atomic.AddInt32(&f.calls, 1)) - 1
	if idx >= len(f.script) {
		idx = len(f.script) - 1
	}
	want := f.script[idx].class

	if f.collector != nil {
		f.collector.Begin()
		defer f.collector.End()
		switch want {
		case ClassRateLimited:
			f.collector.record(signalRecord{statusCode: http.StatusTooManyRequests})
		case ClassAuthFailed:
			f.collector.record(signalRecord{statusCode: http.StatusUnauthorized})
		case ClassContentRejected:
			f.collector.record(signalRecord{statusCode: http.StatusBadRequest})
		case ClassUnreachable:
			f.collector.record(signalRecord{transportErr: true})
		case ClassOK:
			f.collector.record(signalRecord{statusCode: http.StatusOK})
		default:
			f.collector.record(signalRecord{statusCode: http.StatusTeapot})
		}
	}

	switch want {
	case ClassOK:
		return nil
	case ClassRateLimited:
		return errors.New("HTTP 429 Too Many Requests")
	case ClassAuthFailed:
		return errors.New("401 Unauthorized")
	case ClassContentRejected:
		return errors.New("400 Bad Request")
	case ClassUnreachable:
		return errors.New("connection reset by peer")
	default:
		return errors.New("mystery failure")
	}
}

func (f *fakeSender) callCount() int { return int(atomic.LoadInt32(&f.calls)) }

// testConfig returns a fast, deterministic config: backoff delays are tiny
// and the retry budget is generous enough not to interfere.
func testConfig() Config {
	return Config{
		Enabled:          true,
		MaxRetries:       3,
		BackoffInitial:   time.Millisecond,
		BackoffMax:       10 * time.Millisecond,
		MaxRetryTime:     time.Second,
		FailureThreshold: 5,
		Cooldown:         50 * time.Millisecond,
	}
}

// newTestProvider wires a ReliableProvider with an injected clock and a
// sleep that records requested delays without actually waiting.
func newTestProvider(t *testing.T, sender *fakeSender, cfg Config) (*ReliableProvider, *time.Time, *[]time.Duration) {
	t.Helper()
	now := time.Unix(1_700_000_000, 0)
	var slept []time.Duration
	collector := &signalCollector{}
	sender.collector = collector
	p := NewReliableProvider("test", sender, cfg, collector)
	p.now = func() time.Time { return now }
	p.sleep = func(d time.Duration) {
		slept = append(slept, d)
		now = now.Add(d)
	}
	return p, &now, &slept
}

// Scenario 1: rate limiting triggers bounded backoff retry and the retries
// never amplify total request volume beyond 1 + MaxRetries.
func TestRateLimitedBackoffBoundedAndNoAmplification(t *testing.T) {
	sender := &fakeSender{script: []scriptedAttempt{
		{class: ClassRateLimited}, {class: ClassRateLimited},
		{class: ClassRateLimited}, {class: ClassRateLimited},
		{class: ClassOK}, // would only be reached if retries were unbounded
	}}
	cfg := testConfig() // MaxRetries=3 => at most 4 attempts
	p, _, slept := newTestProvider(t, sender, cfg)

	err := p.Send("msg", "")
	require.Error(t, err)
	se := err.(*SendError)
	require.Equal(t, ClassRateLimited, se.Class)
	require.Equal(t, 4, se.Attempts, "1 original + exactly MaxRetries retries")
	require.Equal(t, 4, sender.callCount(), "total requests must not exceed 1+MaxRetries")
	require.Len(t, *slept, 3, "a delay exists between attempts, none after the last")

	// Exponential growth with the configured cap.
	require.Equal(t, time.Millisecond, (*slept)[0])
	require.Equal(t, 2*time.Millisecond, (*slept)[1])
	require.Equal(t, 4*time.Millisecond, (*slept)[2])
}

// Explicit Retry-After must be honored (taken when larger than exponential).
func TestRateLimitedHonorsRetryAfter(t *testing.T) {
	cfg := testConfig()
	now := time.Unix(1_700_000_000, 0)
	var slept []time.Duration
	collector := &signalCollector{}
	sender := &retryAfterSender{collector: collector, untilOK: 4} // never recovers inside MaxRetries
	p := NewReliableProvider("test", sender, cfg, collector)
	p.now = func() time.Time { return now }
	p.sleep = func(d time.Duration) { slept = append(slept, d); now = now.Add(d) }

	err := p.Send("msg", "")
	require.Error(t, err)
	require.Equal(t, ClassRateLimited, err.(*SendError).Class)
	require.NotEmpty(t, slept)
	require.GreaterOrEqual(t, slept[0], 7*time.Millisecond,
		"channel-advertised Retry-After must not be undercut")
}

type retryAfterSender struct {
	collector *signalCollector
	calls     int
	untilOK   int
}

func (s *retryAfterSender) Send(_, _ string) error {
	s.calls++
	s.collector.Begin()
	if s.calls <= s.untilOK {
		s.collector.record(signalRecord{statusCode: http.StatusTooManyRequests, retryAfter: 7 * time.Millisecond})
		s.collector.End()
		return errors.New("429")
	}
	s.collector.record(signalRecord{statusCode: http.StatusOK})
	s.collector.End()
	return nil
}

// Scenario 2: auth failure must not retry — one attempt, immediate alert path.
func TestAuthFailureNoRetry(t *testing.T) {
	sender := &fakeSender{script: []scriptedAttempt{{class: ClassAuthFailed}}}
	cfg := testConfig()
	p, _, slept := newTestProvider(t, sender, cfg)

	err := p.Send("msg", "")
	require.Error(t, err)
	se := err.(*SendError)
	require.Equal(t, ClassAuthFailed, se.Class)
	require.False(t, se.Class.Retryable())
	require.Equal(t, 1, se.Attempts)
	require.Equal(t, 1, sender.callCount(), "auth failure must produce exactly one request")
	require.Empty(t, *slept, "no backoff sleep on a deterministic failure")
}

// Content rejection is also deterministic and must not retry.
func TestContentRejectedNoRetry(t *testing.T) {
	sender := &fakeSender{script: []scriptedAttempt{{class: ClassContentRejected}}}
	p, _, _ := newTestProvider(t, sender, testConfig())

	err := p.Send("msg", "")
	require.Equal(t, ClassContentRejected, err.(*SendError).Class)
	require.Equal(t, 1, sender.callCount())
}

// Transient failure followed by success: retries recover and total attempts
// stay bounded.
func TestUnreachableRecoversWithRetry(t *testing.T) {
	sender := &fakeSender{script: []scriptedAttempt{
		{class: ClassUnreachable}, {class: ClassUnreachable}, {class: ClassOK},
	}}
	p, _, _ := newTestProvider(t, sender, testConfig())

	require.NoError(t, p.Send("msg", ""))
	require.Equal(t, 3, sender.callCount())
	require.Equal(t, StateClosed, p.Breaker().Status().State, "success closes/keeps the breaker closed")
}

// The retry-time budget cuts retries even when MaxRetries would allow more,
// which is what bounds total push wall-clock time.
func TestRetryTimeBudgetCapsAttempts(t *testing.T) {
	sender := &fakeSender{script: []scriptedAttempt{{class: ClassUnreachable}}}
	cfg := testConfig()
	cfg.MaxRetries = 10
	cfg.MaxRetryTime = 500 * time.Microsecond
	cfg.BackoffInitial = time.Millisecond // first delay already exceeds budget
	p, _, slept := newTestProvider(t, sender, cfg)

	err := p.Send("msg", "")
	require.Error(t, err)
	require.Equal(t, 1, sender.callCount(), "no retry fits inside the time budget")
	require.Empty(t, *slept)
}
