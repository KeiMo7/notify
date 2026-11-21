package runner

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/projectdiscovery/notify/pkg/providers"
	"github.com/projectdiscovery/notify/pkg/reliability"
	"github.com/projectdiscovery/notify/pkg/types"
)

// e2eSender is a controllable Provider: flipping failing makes every send
// return a classified unreachable error, which drives the full
// retry -> breaker -> spill-queue path without touching real networks.
type e2eSender struct {
	calls   int32
	failing bool
}

func (s *e2eSender) Send(_, _ string) error {
	atomic.AddInt32(&s.calls, 1)
	if s.failing {
		return errE2EUnreachable
	}
	return nil
}

var errE2EUnreachable = e2eErr("dial tcp: connection reset by peer")

type e2eErr string

func (e e2eErr) Error() string { return string(e) }

func newE2ERunner(t *testing.T, cfg reliability.Config) (*Runner, *e2eSender, string) {
	t.Helper()
	dir := t.TempDir()
	queuePath := filepath.Join(dir, "pending.json")
	cfg.QueuePath = queuePath

	sender := &e2eSender{}
	po := &providers.ProviderOptions{Reliability: cfg}
	client, err := providers.NewForTest(po, &types.Options{}, sender)
	require.NoError(t, err)

	r := &Runner{options: &types.Options{CharLimit: 4000}, providers: client}
	return r, sender, queuePath
}

// Acceptance scenario 4, full path: all channels failing => the input line is
// persisted to the local queue file and is NOT dropped.
func TestRunnerSpillsToLocalQueueWhenAllChannelsDown(t *testing.T) {
	cfg := reliability.Config{
		Enabled:          true,
		MaxRetries:       1,
		BackoffInitial:   1,
		BackoffMax:       1,
		MaxRetryTime:     1000,
		FailureThreshold: 100, // keep breaker closed so every line attempts sends
		Cooldown:         1000,
		QueueMaxEntries:  10,
	}
	r, sender, queuePath := newE2ERunner(t, cfg)
	sender.failing = true

	require.NoError(t, r.sendMessage("critical-result"))

	data, err := os.ReadFile(queuePath)
	require.NoError(t, err)
	require.Contains(t, string(data), "critical-result",
		"message must be on disk in the local queue, not dropped")
	require.GreaterOrEqual(t, atomic.LoadInt32(&sender.calls), int32(1))
}

// Replay: a queue file left by a previous crashed run is delivered as soon as
// the channel is healthy again, then removed from disk.
func TestRunnerReplaysPendingQueueOnRecovery(t *testing.T) {
	cfg := reliability.Config{
		Enabled: true, MaxRetries: 0, FailureThreshold: 100, Cooldown: 1000,
		BackoffInitial: 1, BackoffMax: 1, MaxRetryTime: 1000, QueueMaxEntries: 10,
	}
	r, sender, queuePath := newE2ERunner(t, cfg)

	// Seed a backlog the way an earlier failed run would have.
	engine := r.providers.ReliabilityEngine()
	require.NotNil(t, engine)
	require.NoError(t, engine.Queue().Enqueue(
		reliability.QueueEntry{EnqueuedAt: "2026-01-01T00:00:00Z", Message: "backlogged"}))

	replayed, remaining := engine.ReplayQueue()
	require.Equal(t, 1, replayed)
	require.Zero(t, remaining)

	data, err := os.ReadFile(queuePath)
	require.NoError(t, err)
	require.NotContains(t, string(data), "backlogged", "acked entries must be cleared")
	require.GreaterOrEqual(t, atomic.LoadInt32(&sender.calls), int32(1))
}

// With the switch off, no engine exists and sendMessage keeps legacy
// semantics: a provider error is returned to the caller unchanged.
func TestRunnerLegacyBehaviorWhenSwitchOff(t *testing.T) {
	sender := &e2eSender{failing: true}
	client, err := providers.NewForTest(&providers.ProviderOptions{}, &types.Options{}, sender)
	require.NoError(t, err)
	require.Nil(t, client.ReliabilityEngine())

	r := &Runner{options: &types.Options{CharLimit: 4000}, providers: client}
	// Legacy contract: Client.Send logs the adapter error but always returns
	// nil so one bad channel never stops processing of later input lines.
	err = r.sendMessage("x")
	require.NoError(t, err)
	require.Equal(t, int32(1), atomic.LoadInt32(&sender.calls), "no retries in legacy path")
}
