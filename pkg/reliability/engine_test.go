package reliability

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// httpSender is a real adapter-shaped Sender talking to an httptest server,
// so the whole path (instrumented transport -> classifier -> breaker) is
// exercised with genuine HTTP responses.
type httpSender struct {
	url    string
	calls  int32
	client *http.Client
}

func (h *httpSender) Send(_, _ string) error {
	atomic.AddInt32(&h.calls, 1)
	req, err := http.NewRequest(http.MethodPost, h.url, nil)
	if err != nil {
		return err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return &httpStatusError{status: resp.StatusCode}
	}
	return nil
}

type httpStatusError struct{ status int }

func (e *httpStatusError) Error() string { return "HTTP " + http.StatusText(e.status) }

func newTestEngine(t *testing.T, cfg Config, senders ...Sender) (*Engine, *LocalQueue, string) {
	t.Helper()
	queuePath := filepath.Join(t.TempDir(), "queue.json")
	queue, err := OpenLocalQueue(queuePath, cfg.QueueMaxEntries)
	require.NoError(t, err)

	collector := &signalCollector{}
	wrapped := make([]*ReliableProvider, 0, len(senders))
	for i, s := range senders {
		rp := NewReliableProvider(channelName(i), s, cfg, collector)
		rp.now = time.Now
		rp.sleep = time.Sleep
		wrapped = append(wrapped, rp)
	}
	engine := NewEngine(EngineOptions{Config: cfg, Providers: wrapped, Queue: queue})
	return engine, queue, queuePath
}

func channelName(i int) string {
	names := []string{"discord", "slack", "telegram"}
	if i < len(names) {
		return names[i]
	}
	return "custom"
}

func fastCfg() Config {
	cfg := Config{
		Enabled:          true,
		MaxRetries:       1,
		BackoffInitial:   time.Millisecond,
		BackoffMax:       2 * time.Millisecond,
		MaxRetryTime:     time.Second,
		FailureThreshold: 2,
		Cooldown:         50 * time.Millisecond,
		QueueMaxEntries:  10,
	}
	return cfg
}

// Scenario 4 end-to-end: both channels answer 503, so the message must be
// written to the local queue rather than dropped.
func TestEngineAllProvidersUnavailableSpillsToQueue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable) // no Retry-After: unreachable, not rate-limit
	}))
	defer srv.Close()

	cfg := fastCfg()
	collector := &signalCollector{}
	senderA := &httpSender{url: srv.URL, client: &http.Client{Transport: NewInstrumentedTransport(http.DefaultTransport, collector)}}
	senderB := &httpSender{url: srv.URL, client: &http.Client{Transport: NewInstrumentedTransport(http.DefaultTransport, collector)}}

	engine, queue, _ := newTestEngine(t, cfg, senderA, senderB)
	engine.collector = collector
	for _, p := range engine.providers {
		p.collector = collector
	}

	require.NoError(t, engine.Send("important-result"))
	require.Equal(t, 1, queue.Len(), "message must be queued, not dropped")
	require.Equal(t, "important-result", queue.Drain()[0].Message)
}

// A failing channel must not block the others: one 503 channel and one 200
// channel => no spill, healthy channel delivered.
func TestEngineOneProviderFailureDoesNotBlockOthers(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer bad.Close()
	defer good.Close()

	cfg := fastCfg()
	collector := &signalCollector{}
	badSender := &httpSender{url: bad.URL, client: &http.Client{Transport: NewInstrumentedTransport(http.DefaultTransport, collector)}}
	goodSender := &httpSender{url: good.URL, client: &http.Client{Transport: NewInstrumentedTransport(http.DefaultTransport, collector)}}

	engine, queue, _ := newTestEngine(t, cfg, badSender, goodSender)
	engine.collector = collector
	for _, p := range engine.providers {
		p.collector = collector
	}

	require.NoError(t, engine.Send("hello"))
	require.Zero(t, queue.Len(), "at least one healthy channel means no spill")
	require.GreaterOrEqual(t, atomic.LoadInt32(&goodSender.calls), int32(1))
}

// Rate limited (explicit 429) over HTTP: classification and retry observed
// through the instrumented transport, and total attempts stay bounded.
func TestEngineRateLimitedOverHTTP(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	cfg := fastCfg()
	cfg.MaxRetries = 2
	collector := &signalCollector{}
	sender := &httpSender{url: srv.URL, client: &http.Client{Transport: NewInstrumentedTransport(http.DefaultTransport, collector)}}
	engine, _, _ := newTestEngine(t, cfg, sender)
	engine.collector = collector
	engine.providers[0].collector = collector

	require.NoError(t, engine.Send("x"))
	// 1 original + 2 retries = 3 requests, never more: no amplification.
	require.Equal(t, int32(3), atomic.LoadInt32(&hits))
	require.Equal(t, int64(3), engine.providers[0].Counters().RateLimited)
}

// Replay: queued messages are delivered once a channel recovers, then removed.
func TestEngineReplayAfterRecovery(t *testing.T) {
	var healthy int32
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&healthy, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer good.Close()

	cfg := fastCfg()
	collector := &signalCollector{}
	sender := &httpSender{url: good.URL, client: &http.Client{Transport: NewInstrumentedTransport(http.DefaultTransport, collector)}}
	engine, queue, _ := newTestEngine(t, cfg, sender)
	engine.collector = collector
	engine.providers[0].collector = collector

	require.NoError(t, queue.Enqueue(QueueEntry{EnqueuedAt: "past", Message: "backlog-1"}))
	require.NoError(t, queue.Enqueue(QueueEntry{EnqueuedAt: "past", Message: "backlog-2"}))

	replayed, remaining := engine.ReplayQueue()
	require.Equal(t, 2, replayed)
	require.Zero(t, remaining)
	require.Zero(t, queue.Len())
	require.GreaterOrEqual(t, atomic.LoadInt32(&healthy), int32(2))
}

// Dedup summary reaches the providers as a single merged push.
func TestEngineDedupSummary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := fastCfg()
	cfg.DedupWindow = time.Hour
	collector := &signalCollector{}
	sender := &httpSender{url: srv.URL, client: &http.Client{Transport: NewInstrumentedTransport(http.DefaultTransport, collector)}}
	engine, queue, _ := newTestEngine(t, cfg, sender)
	engine.collector = collector
	engine.providers[0].collector = collector

	require.NoError(t, engine.Send("dup")) // fresh -> sent
	require.NoError(t, engine.Send("dup")) // merged
	require.NoError(t, engine.Send("dup")) // merged
	require.Equal(t, int32(1), atomic.LoadInt32(&sender.calls))

	// Close flushes the coalesced window as a single summary push.
	engine.Close()
	require.Equal(t, int32(2), atomic.LoadInt32(&sender.calls))
	require.Zero(t, queue.Len())
}

// Status snapshot exposes degraded channels and failure counts.
func TestEngineStatusObservable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	cfg := fastCfg()
	cfg.FailureThreshold = 1 // auth failure alone opens the breaker
	collector := &signalCollector{}
	sender := &httpSender{url: srv.URL, client: &http.Client{Transport: NewInstrumentedTransport(http.DefaultTransport, collector)}}
	engine, _, _ := newTestEngine(t, cfg, sender)
	engine.collector = collector
	engine.providers[0].collector = collector

	require.NoError(t, engine.Send("x"))
	status := engine.Status()
	require.Len(t, status.Providers, 1)
	require.Equal(t, StateOpen, status.Providers[0].State)
	require.GreaterOrEqual(t, status.Providers[0].Counters.AuthFailed, int64(1))
}
