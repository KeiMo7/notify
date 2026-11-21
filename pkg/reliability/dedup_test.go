package reliability

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDeduperMergesWithinWindow(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clock := base
	d := NewDeduper(10 * time.Second)
	d.now = func() time.Time { return clock }

	// First occurrence: deliver.
	d1 := d.Admit("same-result")
	require.True(t, d1.Fresh)
	require.Empty(t, d1.DueSummaries)

	// Repeated occurrences inside the window: swallowed, not re-sent.
	d2 := d.Admit("same-result")
	require.False(t, d2.Fresh)
	require.Equal(t, 1, d2.Merged)

	d3 := d.Admit("same-result")
	require.False(t, d3.Fresh)
	require.Equal(t, 2, d3.Merged)

	// A different result is independent.
	d4 := d.Admit("other-result")
	require.True(t, d4.Fresh)

	// Window elapses: next Admit flushes exactly one summary for the
	// repeated result, then the fresh message goes through normally.
	clock = clock.Add(11 * time.Second)
	d5 := d.Admit("same-result")
	require.True(t, d5.Fresh)
	require.Len(t, d5.DueSummaries, 1)
	require.Equal(t, "same-result", d5.DueSummaries[0].Message)
	require.Equal(t, 3, d5.DueSummaries[0].Count)
}

// A window containing a single occurrence must not emit a summary: that
// message was already delivered when it first arrived.
func TestDeduperNoSummaryForSingleton(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clock := base
	d := NewDeduper(time.Second)
	d.now = func() time.Time { return clock }

	require.True(t, d.Admit("once").Fresh)
	clock = clock.Add(2 * time.Second)
	decision := d.Admit("unrelated")
	require.Empty(t, decision.DueSummaries)
}

func TestDeduperDisabledByZeroWindow(t *testing.T) {
	d := NewDeduper(0)
	for i := 0; i < 5; i++ {
		require.True(t, d.Admit("x").Fresh)
	}
	require.Nil(t, d.Flush())
}

func TestDeduperFlushAtShutdown(t *testing.T) {
	d := NewDeduper(time.Minute)
	require.True(t, d.Admit("x").Fresh)
	require.False(t, d.Admit("x").Fresh)
	require.False(t, d.Admit("x").Fresh)

	out := d.Flush()
	require.Len(t, out, 1)
	require.Equal(t, 3, out[0].Count)
	require.Empty(t, d.Flush(), "flush is one-shot")
}
