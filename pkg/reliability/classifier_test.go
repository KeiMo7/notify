package reliability

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClassifyExplicitHTTPSignals(t *testing.T) {
	cases := []struct {
		name string
		rec  signalRecord
		want FailureClass
	}{
		{"429 is rate limited", signalRecord{statusCode: http.StatusTooManyRequests}, ClassRateLimited},
		{"401 is auth failure", signalRecord{statusCode: http.StatusUnauthorized}, ClassAuthFailed},
		{"403 is auth failure", signalRecord{statusCode: http.StatusForbidden}, ClassAuthFailed},
		{"400 is content rejection", signalRecord{statusCode: http.StatusBadRequest}, ClassContentRejected},
		{"413 is content rejection", signalRecord{statusCode: http.StatusRequestEntityTooLarge}, ClassContentRejected},
		{"500 is unreachable", signalRecord{statusCode: http.StatusInternalServerError}, ClassUnreachable},
		{"503 without Retry-After is unreachable, NOT rate limit", signalRecord{statusCode: http.StatusServiceUnavailable}, ClassUnreachable},
		{"503 with Retry-After is rate limited", signalRecord{statusCode: http.StatusServiceUnavailable, retryAfter: 2 * time.Second}, ClassRateLimited},
		{"404 is unreachable", signalRecord{statusCode: http.StatusNotFound}, ClassUnreachable},
		{"transport error is unreachable", signalRecord{transportErr: true}, ClassUnreachable},
		{"other 2xx/3xx cannot be classified", signalRecord{statusCode: http.StatusOK}, ClassUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := Classify([]signalRecord{tc.rec}, errors.New("adapter error"))
			require.Equal(t, tc.want, got)
		})
	}
}

// Hard constraint: rate limiting must come from an explicit channel signal,
// never from latency. A slow-but-successful response classifies as OK/Unknown,
// never as rate limited.
func TestClassifyNeverInfersRateLimitFromLatency(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	collector := &signalCollector{}
	client := &http.Client{Transport: NewInstrumentedTransport(http.DefaultTransport, collector)}

	collector.Begin()
	resp, err := client.Get(srv.URL)
	records := collector.End()
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	class, _ := Classify(records, nil)
	require.NotEqual(t, ClassRateLimited, class, "latency alone must never imply rate limiting")
}

func TestClassifyRetryAfterPropagation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	collector := &signalCollector{}
	client := &http.Client{Transport: NewInstrumentedTransport(http.DefaultTransport, collector)}

	collector.Begin()
	resp, err := client.Get(srv.URL)
	require.NoError(t, err)
	_ = resp.Body.Close()
	records := collector.End()

	class, retryAfter := Classify(records, errors.New("send failed"))
	require.Equal(t, ClassRateLimited, class)
	require.Equal(t, 3*time.Second, retryAfter)
}

func TestParseRetryAfterMalformed(t *testing.T) {
	// Garbage must yield zero, never a panic and never an unbounded sleep.
	require.Equal(t, time.Duration(0), parseRetryAfter("not-a-date"))
	require.Equal(t, time.Duration(0), parseRetryAfter(""))
	require.Equal(t, 2*time.Second, parseRetryAfter("2"))
}

func TestClassifyErrorTextFallback(t *testing.T) {
	cases := []struct {
		text string
		want FailureClass
	}{
		{"HTTP 429 Too Many Requests", ClassRateLimited},
		{"rate limit exceeded for token", ClassRateLimited},
		{"401 Unauthorized: invalid token", ClassAuthFailed},
		{"smtp: 535 5.7.8 Authentication credentials invalid", ClassAuthFailed},
		{"400 Bad Request: payload too large", ClassContentRejected},
		{"dial tcp: connection reset by peer", ClassUnreachable},
		{"no such host", ClassUnreachable},
		{"something completely different", ClassUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			require.Equal(t, tc.want, classifyFromError(errors.New(tc.text)))
		})
	}
}

// Connection reset: the instrumented transport must record a transport error
// and keep returning the original error untouched (deterministic, no swallow).
func TestInstrumentedTransportConnectionReset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		require.True(t, ok)
		conn, _, _ := hj.Hijack()
		_ = conn.Close() // abrupt reset without an HTTP response
	}))
	defer srv.Close()

	collector := &signalCollector{}
	client := &http.Client{Transport: NewInstrumentedTransport(http.DefaultTransport, collector)}

	collector.Begin()
	resp, err := client.Get(srv.URL)
	records := collector.End()
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err)
	require.Len(t, records, 1)
	require.True(t, records[0].transportErr)

	class, _ := Classify(records, err)
	require.Equal(t, ClassUnreachable, class)
}

// Records outside an active window are dropped rather than misattributed, and
// nested windows (adapter wrapping its own Begin/End inside the executor's)
// preserve the outer observations.
func TestSignalCollectorWindowIsolation(t *testing.T) {
	c := &signalCollector{}
	c.record(signalRecord{statusCode: 500}) // no active window: ignored
	c.Begin()
	c.record(signalRecord{statusCode: 429})
	c.Begin()                // nested adapter window
	c.record(signalRecord{}) // nested observation
	require.Nil(t, c.End())  // nested End returns nothing, keeps state
	recs := c.End()
	require.Len(t, recs, 2)
	require.Equal(t, 429, recs[0].statusCode)
}
