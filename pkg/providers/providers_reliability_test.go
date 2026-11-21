package providers

import (
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/projectdiscovery/notify/pkg/reliability"
	"github.com/projectdiscovery/notify/pkg/types"
)

// stubProvider is a minimal Provider for wiring tests; it records the exact
// messages and format it received so legacy-path parity can be asserted.
type stubProvider struct {
	calls   int32
	message string
	format  string
	failing bool
}

func (s *stubProvider) Send(message, cliFormat string) error {
	atomic.AddInt32(&s.calls, 1)
	s.message = message
	s.format = cliFormat
	return nil
}

func baseOptions() *types.Options {
	return &types.Options{MessageFormat: "{{data}}"}
}

// Acceptance: with the feature disabled the engine is never built and
// Client.Send follows the exact legacy sequence — one direct adapter call
// per message, no wrapping, no queue.
func TestSendLegacyPathWhenDisabled(t *testing.T) {
	stub := &stubProvider{}
	client := &Client{
		providers:       []Provider{stub},
		providerOptions: &ProviderOptions{},
		options:         baseOptions(),
	}
	require.Nil(t, client.ReliabilityEngine())

	require.NoError(t, client.Send("hello"))
	require.Equal(t, int32(1), atomic.LoadInt32(&stub.calls))
	require.Equal(t, "hello", stub.message)
	require.Equal(t, "{{data}}", stub.format)

	require.NoError(t, client.Send("world"))
	require.Equal(t, int32(2), atomic.LoadInt32(&stub.calls))
}

// An illegal reliability section must fail construction with the offending
// field named, before any notification is attempted.
func TestInvalidReliabilityConfigRejectedAtStartup(t *testing.T) {
	po := &ProviderOptions{Reliability: reliability.Config{Enabled: true, MaxRetries: -3}}
	client, err := NewForTest(po, baseOptions(), &stubProvider{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "max_retries")
	require.Nil(t, client)
}

// Enabled mode wires every provider into a ReliableProvider and opens the
// spill queue on disk.
func TestEnabledModeWrapsProvidersAndQueue(t *testing.T) {
	queuePath := filepath.Join(t.TempDir(), "queue.json")
	client := &Client{
		providers: []Provider{&stubProvider{}, &stubProvider{}},
		providerOptions: &ProviderOptions{Reliability: reliability.Config{
			Enabled:         true,
			QueuePath:       queuePath,
			QueueMaxEntries: 7,
		}},
		options: baseOptions(),
	}
	require.NoError(t, client.initReliability(&client.providerOptions.Reliability))

	engine := client.ReliabilityEngine()
	require.NotNil(t, engine)
	status := engine.Status()
	require.Len(t, status.Providers, 2)
	for _, row := range status.Providers {
		require.Equal(t, reliability.StateClosed, row.State, "all breakers start closed")
	}
	require.Zero(t, status.Queue.Length, "fresh queue starts empty")

	// A successful send reaches the underlying adapter unchanged.
	require.NoError(t, client.Send("one"))
	require.Equal(t, int64(1), engine.Status().Providers[0].Counters.Attempts)
}

// Provider naming helper must produce stable lowercase adapter names.
func TestProviderName(t *testing.T) {
	require.Equal(t, "stubprovider", providerName(&stubProvider{}))
}
