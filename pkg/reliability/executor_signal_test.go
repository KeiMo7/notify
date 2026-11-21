package reliability

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// statusIgnoringSender mimics the custom webhook adapter: it performs the
// HTTP call through the instrumented transport but returns nil regardless of
// the response status. The reliability layer must still react to the explicit
// channel signal the transport observed.
type statusIgnoringSender struct {
	collector *signalCollector
	status    int
	calls     int
}

func (s *statusIgnoringSender) Send(_, _ string) error {
	s.calls++
	s.collector.Begin()
	s.collector.record(signalRecord{statusCode: s.status})
	s.collector.End()
	return nil // adapter deliberately ignores status code
}

// A 429 ignored by the adapter must still be classified and retried, bounded.
func TestAdapterNilErrorButChannel429StillRetried(t *testing.T) {
	cfg := testConfig()
	cfg.MaxRetries = 2
	collector := &signalCollector{}
	sender := &statusIgnoringSender{collector: collector, status: http.StatusTooManyRequests}
	p := NewReliableProvider("custom", sender, cfg, collector)
	p.now = time.Now
	p.sleep = time.Sleep

	err := p.Send("m", "")
	require.Error(t, err)
	require.Equal(t, ClassRateLimited, err.(*SendError).Class)
	require.Equal(t, 3, sender.calls, "1 original + 2 retries despite adapter returning nil")
	require.Equal(t, int64(3), p.Counters().RateLimited)
}

// A 401 ignored by the adapter must still be a no-retry auth failure.
func TestAdapterNilErrorButChannel401NoRetry(t *testing.T) {
	cfg := testConfig()
	collector := &signalCollector{}
	sender := &statusIgnoringSender{collector: collector, status: http.StatusUnauthorized}
	p := NewReliableProvider("custom", sender, cfg, collector)

	err := p.Send("m", "")
	require.Error(t, err)
	require.Equal(t, ClassAuthFailed, err.(*SendError).Class)
	require.Equal(t, 1, sender.calls)
}

// Sanity: an adapter returning a plain error with no records still uses the
// error-text fallback rather than the signaled-failure synthesis.
func TestPlainErrorWithoutRecordsUsesFallback(t *testing.T) {
	cfg := testConfig()
	cfg.MaxRetries = 0
	p := NewReliableProvider("x", &plainErrSender{errors.New("401 Unauthorized")}, cfg, &signalCollector{})
	err := p.Send("m", "")
	require.Equal(t, ClassAuthFailed, err.(*SendError).Class)
}

type plainErrSender struct{ err error }

func (s plainErrSender) Send(_, _ string) error { return s.err }
