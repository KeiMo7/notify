package reliability

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Malformed upstream data: the server sends an invalid/unknown HTTP status
// line fragment by hijacking the connection. The path must remain
// deterministic — a transport error classified as unreachable, never a panic,
// and the original error is propagated unchanged.
func TestInstrumentedTransportMalformedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		require.True(t, ok)
		conn, _, _ := hj.Hijack()
		// Write garbage that is not a valid HTTP response.
		_, _ = conn.Write([]byte("GARBAGE-NOT-HTTP\r\n\r\n"))
		_ = conn.Close()
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
	require.Error(t, err, "malformed response must surface as an error, not be swallowed")
	require.NotEmpty(t, records)
	require.True(t, records[len(records)-1].transportErr,
		"the malformed reply is recorded as a transport-level failure")

	class, _ := Classify(records, err)
	require.Equal(t, ClassUnreachable, class, "garbled upstream data is an unreachable, retryable condition")
}

// A response with a valid status but an unreadable/aborted body must not
// crash the collector; classification still relies on the explicit status.
func TestInstrumentedTransportAbortedBodyKeepsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		hj, _ := w.(http.Hijacker)
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close() // abort before a complete body
		}
	}))
	defer srv.Close()

	collector := &signalCollector{}
	client := &http.Client{Transport: NewInstrumentedTransport(http.DefaultTransport, collector)}

	collector.Begin()
	resp, _ := client.Get(srv.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	records := collector.End()

	class, retryAfter := Classify(records, nil)
	require.Equal(t, ClassRateLimited, class, "explicit 429 still wins despite the aborted body")
	require.Equal(t, time.Second, retryAfter)
}
