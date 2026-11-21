package reliability

import (
	"fmt"
	"strings"
	"time"

	"github.com/projectdiscovery/gologger"
)

// Engine owns the reliability machinery for one providers.Client: the wrapped
// providers, the deduper, the spill queue and the shared signal collector.
// It deliberately preserves the legacy dispatch order — providers are tried
// sequentially and one provider's failure never blocks the others — and adds
// no goroutines, so behavior stays deterministic.
type Engine struct {
	cfg       Config
	providers []*ReliableProvider
	dedup     *Deduper
	queue     *LocalQueue
	collector *signalCollector
	cliFormat string

	now func() time.Time
}

// EngineOptions bundles the construction dependencies.
type EngineOptions struct {
	Config        Config
	Providers     []*ReliableProvider
	Queue         *LocalQueue
	MessageFormat string
}

// NewEngine assembles the engine from an already-validated, defaulted config.
func NewEngine(opts EngineOptions) *Engine {
	collector := &signalCollector{}
	for _, p := range opts.Providers {
		p.collector = collector
	}
	return &Engine{
		cfg:       opts.Config,
		providers: opts.Providers,
		dedup:     NewDeduper(opts.Config.DedupWindow),
		queue:     opts.Queue,
		collector: collector,
		cliFormat: opts.MessageFormat,
		now:       time.Now,
	}
}

// Collector exposes the HTTP signal collector so the runner can install the
// instrumented transport on http.DefaultClient.
func (e *Engine) Collector() *signalCollector { return e.collector }

// Queue exposes the spill queue; used by tests that seed a backlog and by
// callers that want to inspect pending/dropped counters.
func (e *Engine) Queue() *LocalQueue { return e.queue }

// ProviderStatus is one row of the observability snapshot.
type ProviderStatus struct {
	Name             string
	State            BreakerState
	ConsecutiveFails int
	TotalFails       int64
	Counters         Counters
}

// QueueStatus describes the spill queue.
type QueueStatus struct {
	Path    string
	Length  int
	Dropped int64
}

// StatusReport is the point-in-time view printed on demand and at warn time.
type StatusReport struct {
	Providers []ProviderStatus
	Queue     QueueStatus
}

// Status snapshots every breaker and counter plus the queue position.
func (e *Engine) Status() StatusReport {
	report := StatusReport{
		Providers: make([]ProviderStatus, 0, len(e.providers)),
	}
	for _, p := range e.providers {
		snap := p.Breaker().Status()
		report.Providers = append(report.Providers, ProviderStatus{
			Name:             p.Name(),
			State:            snap.State,
			ConsecutiveFails: snap.ConsecutiveFails,
			TotalFails:       snap.TotalFails,
			Counters:         p.Counters(),
		})
	}
	if e.queue != nil {
		report.Queue = QueueStatus{
			Path:    e.queue.Path(),
			Length:  e.queue.Len(),
			Dropped: e.queue.Dropped(),
		}
	}
	return report
}

// LogStatus writes the snapshot through gologger so operators can see which
// channels are degraded and the failure counts behind each decision.
func (e *Engine) LogStatus() {
	report := e.Status()
	for _, row := range report.Providers {
		gologger.Warning().Msgf(
			"reliability: provider=%s state=%s consecutive_failures=%d total_failures=%d retries=%d rate_limited=%d auth_failed=%d content_rejected=%d unreachable=%d skipped_while_open=%d",
			row.Name, row.State, row.ConsecutiveFails, row.TotalFails,
			row.Counters.Retries, row.Counters.RateLimited, row.Counters.AuthFailed,
			row.Counters.ContentReject, row.Counters.Unreachable, row.Counters.OpenSkipped,
		)
	}
	gologger.Warning().Msgf("reliability: spill queue path=%s length=%d dropped_oldest=%d",
		report.Queue.Path, report.Queue.Length, report.Queue.Dropped)
}

// Send applies dedup, then dispatches to every provider. When every provider
// fails the message is spilled to the local queue instead of being dropped.
// The method returns nil to preserve the legacy Client.Send contract: all
// failures are surfaced through logs and structured status, never as a fatal
// error that would stop processing of subsequent input lines.
func (e *Engine) Send(message string) error {
	decision := e.dedup.Admit(message)

	// Windows that closed since the last call emit one summary each.
	for _, summary := range decision.DueSummaries {
		e.dispatch(summaryMessage(summary))
	}

	if !decision.Fresh {
		gologger.Info().Msgf("reliability: duplicate merged (+%d suppressed within window %s)",
			decision.Merged, e.cfg.DedupWindow)
		return nil
	}

	e.dispatch(message)
	return nil
}

// dispatch runs one message through every provider and decides whether it
// must be spilled. Sequential on purpose: matches the legacy ordering, a slow
// or broken provider cannot starve the others, and there is no shared-state
// race to reason about.
func (e *Engine) dispatch(message string) {
	failures := make([]*SendError, 0, len(e.providers))

	for _, p := range e.providers {
		err := p.Send(message, e.cliFormat)
		if err == nil {
			continue
		}
		sendErr, ok := err.(*SendError)
		if !ok {
			// Defensive: ReliableProvider only ever returns *SendError.
			sendErr = &SendError{Provider: p.Name(), Class: ClassUnknown, Attempts: 1, LastErr: err}
		}
		failures = append(failures, sendErr)
		e.alert(sendErr)
	}

	if len(e.providers) > 0 && len(failures) == len(e.providers) {
		// Every configured provider failed or was degraded: do not drop.
		e.spill(message)
	}
}

// alert turns a provider failure into the right operator signal. Auth and
// content rejection are deterministic and page immediately; a breaker
// opening is a degradation event; ordinary retried failures stay at verbose
// level to avoid the notification storm this feature exists to prevent.
func (e *Engine) alert(sendErr *SendError) {
	switch sendErr.Class {
	case ClassAuthFailed:
		gologger.Error().Msgf("reliability: AUTH FAILURE on provider %q, not retrying: %v",
			sendErr.Provider, sendErr.LastErr)
	case ClassContentRejected:
		gologger.Error().Msgf("reliability: CONTENT REJECTED by provider %q, not retrying: %v",
			sendErr.Provider, sendErr.LastErr)
	case ClassRateLimited:
		gologger.Warning().Msgf("reliability: rate limited by provider %q after %d attempt(s), backing off: %v",
			sendErr.Provider, sendErr.Attempts, sendErr.LastErr)
	default:
		gologger.Verbose().Msgf("reliability: provider %q failed (class=%s, attempts=%d): %v",
			sendErr.Provider, sendErr.Class, sendErr.Attempts, sendErr.LastErr)
	}
	if sendErr.Open {
		gologger.Warning().Msgf("reliability: provider %q DEGRADED — circuit open for %s, other providers continue",
			sendErr.Provider, e.cfg.Cooldown)
		e.LogStatus()
	}
}

// spill persists a message that no provider could accept.
func (e *Engine) spill(message string) {
	if e.queue == nil {
		gologger.Error().Msgf("reliability: all providers failed and no spill queue configured, message lost: %.200s", message)
		return
	}
	before := e.queue.Len()
	err := e.queue.Enqueue(QueueEntry{
		EnqueuedAt: e.now().UTC().Format(time.RFC3339Nano),
		Message:    message,
	})
	if err != nil {
		gologger.Error().Msgf("reliability: failed to persist spill queue entry: %v", err)
		return
	}
	if e.queue.Len() < before+1 {
		gologger.Warning().Msgf("reliability: spill queue full (cap=%d), oldest entries dropped; total dropped=%d",
			e.cfg.QueueMaxEntries, e.queue.Dropped())
	}
	gologger.Warning().Msgf("reliability: all providers unavailable, message stored in local queue (%s, pending=%d)",
		e.queue.Path(), e.queue.Len())
}

// ReplayQueue attempts to deliver everything persisted in the spill queue.
// Entries are acknowledged one by one as soon as at least one provider
// accepts them. The first entry that still cannot reach any provider stops
// the replay and leaves the remainder on disk for the next start.
func (e *Engine) ReplayQueue() (replayed int, remaining int) {
	if e.queue == nil {
		return 0, 0
	}
	entries := e.queue.Drain()
	for i, entry := range entries {
		if allFailed := e.tryOne(entry.Message); allFailed {
			// Keep this entry and everything behind it; ack only the prefix.
			if err := e.queue.Acknowledge(i); err != nil {
				gologger.Error().Msgf("reliability: failed to acknowledge replayed queue entries: %v", err)
			}
			gologger.Warning().Msgf("reliability: replay stops at queue position %d, %d entries remain pending",
				i, len(entries)-i)
			return i, len(entries) - i
		}
		gologger.Info().Msgf("reliability: replayed queued message from %s", entry.EnqueuedAt)
	}
	if err := e.queue.Acknowledge(len(entries)); err != nil {
		gologger.Error().Msgf("reliability: failed to clear replay queue: %v", err)
	}
	return len(entries), e.queue.Len()
}

// tryOne dispatches without touching the deduper (queued messages were
// admitted already) and reports whether every provider failed.
func (e *Engine) tryOne(message string) (allFailed bool) {
	failed := 0
	for _, p := range e.providers {
		if err := p.Send(message, e.cliFormat); err != nil {
			if sendErr, ok := err.(*SendError); ok {
				e.alert(sendErr)
			}
			failed++
		}
	}
	return len(e.providers) > 0 && failed == len(e.providers)
}

// Close flushes coalesced duplicates. Summaries that cannot be delivered are
// spilled like any other message, so shutdown never silently loses data.
func (e *Engine) Close() {
	for _, summary := range e.dedup.Flush() {
		e.dispatch(summaryMessage(summary))
	}
}

// summaryMessage renders the single push that replaces suppressed duplicates.
// Normal one-off messages keep their original format; only coalesced windows
// use this new wrapper.
func summaryMessage(s Summary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[notify] %d duplicate notifications merged within the dedup window:\n", s.Count)
	b.WriteString(s.Message)
	return b.String()
}
