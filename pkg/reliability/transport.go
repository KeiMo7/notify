package reliability

import (
	"net/http"
	"sync"
)

// signalCollector accumulates HTTP round-trip observations for the duration
// of one provider send attempt. The send pipeline in providers.Client.Send is
// strictly sequential (one provider after another, no goroutines), so a
// single collector brackets one attempt at a time and needs no goroutine-local
// storage.
//
// Windows nest safely: an adapter (or a test fake) may itself call Begin/End
// around its HTTP work inside the executor's outer window. Only the outermost
// Begin clears state and only the outermost End returns the records, so
// nested calls cannot wipe observations attributed to the current attempt.
type signalCollector struct {
	mu      sync.Mutex
	records []signalRecord
	depth   int
}

// Begin opens an observation window. The outermost call discards records
// left by a previous, unclosed window so leaked state cannot contaminate the
// next send; nested calls only increment the depth.
func (c *signalCollector) Begin() {
	c.mu.Lock()
	if c.depth == 0 {
		c.records = nil
	}
	c.depth++
	c.mu.Unlock()
}

// End closes a window and, at the outermost level, returns the collected
// records. Nested Ends return nil without touching the accumulated records.
func (c *signalCollector) End() []signalRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.depth > 0 {
		c.depth--
	}
	if c.depth != 0 {
		return nil
	}
	out := c.records
	c.records = nil
	return out
}

// record is invoked from inside RoundTrip (or a test fake); it is a no-op
// outside any window.
func (c *signalCollector) record(rec signalRecord) {
	c.mu.Lock()
	if c.depth > 0 {
		c.records = append(c.records, rec)
	}
	c.mu.Unlock()
}

// InstrumentedTransport wraps an http.RoundTripper and feeds every response
// (status code, Retry-After) and every transport-level error into the
// collector. It does not alter the request or the response: no extra copy,
// no buffering, and the body is passed through untouched.
type InstrumentedTransport struct {
	inner     http.RoundTripper
	collector *signalCollector
}

// NewInstrumentedTransport wraps inner. If inner is nil the standard
// library default transport is used, mirroring net/http semantics.
func NewInstrumentedTransport(inner http.RoundTripper, collector *signalCollector) *InstrumentedTransport {
	if inner == nil {
		inner = http.DefaultTransport
	}
	return &InstrumentedTransport{inner: inner, collector: collector}
}

// RoundTrip implements http.RoundTripper.
func (t *InstrumentedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.inner.RoundTrip(req)
	if err != nil {
		// Connection reset, DNS error, malformed reply surfaced as error.
		t.collector.record(signalRecord{transportErr: true})
		return resp, err
	}

	rec := signalRecord{statusCode: resp.StatusCode}
	if resp.StatusCode == http.StatusTooManyRequests ||
		resp.StatusCode == http.StatusServiceUnavailable {
		rec.retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
	}
	t.collector.record(rec)
	return resp, nil
}
