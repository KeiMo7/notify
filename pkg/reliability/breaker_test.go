package reliability

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Scenario 3: sustained failure opens the circuit (degradation); while open,
// sends are skipped with zero network attempts, and recovery is probed after
// the cooldown.
func TestBreakerDegradesAndRecovers(t *testing.T) {
	cfg := testConfig() // FailureThreshold=5, Cooldown=50ms
	base := time.Unix(1_700_000_000, 0)
	clock := base
	b := NewBreaker(cfg)

	// Four failures stay closed.
	for i := 0; i < cfg.FailureThreshold-1; i++ {
		require.True(t, b.Allow(clock))
		require.False(t, b.OnFailure(clock))
	}
	require.Equal(t, StateClosed, b.Status().State)

	// The threshold failure opens the circuit and reports the transition.
	require.True(t, b.Allow(clock))
	opened := b.OnFailure(clock)
	require.True(t, opened)
	require.Equal(t, StateOpen, b.Status().State)
	require.Equal(t, cfg.FailureThreshold, b.Status().ConsecutiveFails)

	// While open: rejected with no attempt, no matter how many sends arrive.
	clock = clock.Add(cfg.Cooldown - time.Millisecond)
	require.False(t, b.Allow(clock))
	require.False(t, b.Allow(clock))

	// After cooldown: exactly one probe is admitted into half-open.
	clock = clock.Add(2 * time.Millisecond)
	require.True(t, b.Allow(clock))
	require.Equal(t, StateHalfOpen, b.Status().State)
	require.False(t, b.Allow(clock), "a second caller must not also probe")

	// A failed probe re-opens immediately and restarts the cooldown.
	require.True(t, b.OnFailure(clock))
	require.Equal(t, StateOpen, b.Status().State)

	// Next cooldown window: probe succeeds and restores the provider.
	clock = clock.Add(cfg.Cooldown)
	require.True(t, b.Allow(clock))
	b.OnSuccess()
	require.Equal(t, StateClosed, b.Status().State)
	require.Equal(t, 0, b.Status().ConsecutiveFails)
}

// End-to-end through ReliableProvider: once the breaker opens, later sends
// cost zero requests until the cooldown probe succeeds.
func TestReliableProviderSkipsWhileDegraded(t *testing.T) {
	sender := &fakeSender{script: []scriptedAttempt{{class: ClassUnreachable}}}
	cfg := testConfig()
	cfg.FailureThreshold = 2
	cfg.MaxRetries = 0 // isolate breaker behavior: no retries
	p, now, _ := newTestProvider(t, sender, cfg)

	// Two failing sends trip the breaker.
	require.Error(t, p.Send("a", ""))
	require.Error(t, p.Send("b", ""))
	require.Equal(t, StateOpen, p.Breaker().Status().State)
	callsAtOpen := sender.callCount()

	// Degraded sends never touch the channel.
	require.Error(t, p.Send("c", ""))
	require.Error(t, p.Send("d", ""))
	require.Equal(t, callsAtOpen, sender.callCount(), "open circuit must emit zero requests")
	require.GreaterOrEqual(t, p.Counters().OpenSkipped, int64(2))

	// Cooldown passes; the probe goes through and, on success, restores it.
	*now = now.Add(cfg.Cooldown)
	sender.script = []scriptedAttempt{{class: ClassOK}}
	require.NoError(t, p.Send("e", ""))
	require.Equal(t, StateClosed, p.Breaker().Status().State)
}

// A success resets the consecutive-failure streak so isolated incidents do
// not accumulate toward degradation.
func TestBreakerSuccessResetsStreak(t *testing.T) {
	cfg := testConfig()
	cfg.FailureThreshold = 3
	b := NewBreaker(cfg)
	now := time.Unix(1_700_000_000, 0)

	b.OnFailure(now)
	b.OnFailure(now)
	b.OnSuccess()
	b.OnFailure(now)
	b.OnFailure(now)
	require.Equal(t, StateClosed, b.Status().State, "streak was reset by the success")
	require.Equal(t, 2, b.Status().ConsecutiveFails)
}
