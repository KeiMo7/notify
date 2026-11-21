package reliability

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestValidateDisabledIsAlwaysValid(t *testing.T) {
	// Whatever values are present, a disabled section cannot break startup.
	require.NoError(t, (Config{Enabled: false, MaxRetries: -1}).Validate())
}

func TestValidateRejectsIllegalFieldsWithName(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"negative retries", Config{Enabled: true, MaxRetries: -1}, "max_retries"},
		{"negative backoff", Config{Enabled: true, BackoffInitial: -time.Second}, "backoff_initial"},
		{"initial above max", Config{Enabled: true, BackoffInitial: 10 * time.Second, BackoffMax: time.Second}, "backoff_max"},
		{"initial above retry time", Config{Enabled: true, BackoffInitial: 10 * time.Second, MaxRetryTime: time.Second}, "max_retry_time"},
		{"negative threshold", Config{Enabled: true, FailureThreshold: -1}, "failure_threshold"},
		{"negative cooldown", Config{Enabled: true, Cooldown: -time.Second}, "cooldown"},
		{"negative dedup window", Config{Enabled: true, DedupWindow: -time.Second}, "dedup_window"},
		{"negative queue cap", Config{Enabled: true, QueueMaxEntries: -1}, "queue_max_entries"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want, "error must name the offending field")
		})
	}
}

func TestDefaultsApplied(t *testing.T) {
	cfg := Config{Enabled: true}.Defaults()
	require.Equal(t, defaultMaxRetries, cfg.MaxRetries)
	require.Equal(t, defaultBackoffInitial, cfg.BackoffInitial)
	require.Equal(t, defaultBackoffMax, cfg.BackoffMax)
	require.Equal(t, defaultMaxRetryTime, cfg.MaxRetryTime)
	require.Equal(t, defaultFailureThreshold, cfg.FailureThreshold)
	require.Equal(t, defaultCooldown, cfg.Cooldown)
	require.Equal(t, defaultQueueMaxEntries, cfg.QueueMaxEntries)
	require.Zero(t, cfg.DedupWindow, "dedup stays opt-in inside the enabled feature")
}

func TestFailureClassRetryability(t *testing.T) {
	require.True(t, ClassRateLimited.Retryable())
	require.True(t, ClassUnreachable.Retryable())
	require.True(t, ClassUnknown.Retryable())
	require.False(t, ClassAuthFailed.Retryable())
	require.False(t, ClassContentRejected.Retryable())
	require.False(t, ClassOK.Retryable())
}
