package dedup

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func openTemp(t *testing.T, opts Options) *Store {
	t.Helper()
	if opts.Path == "" {
		opts.Path = filepath.Join(t.TempDir(), "dedup-state.json")
	}
	s, err := Open(opts)
	require.NoError(t, err)
	return s
}

func TestCheckNewThenNone(t *testing.T) {
	s := openTemp(t, Options{})

	d := s.Check("host1 open:80")
	require.Equal(t, ChangeNew, d.Kind)

	d = s.Check("host1 open:80")
	require.Equal(t, ChangeNone, d.Kind, "same content must not be reported as new again")
}

func TestCheckContentChange(t *testing.T) {
	s := openTemp(t, Options{KeyRegex: `^(\S+)`})

	require.Equal(t, ChangeNew, s.Check("host1 critical").Kind)
	d := s.Check("host1 high")
	require.Equal(t, ChangeContent, d.Kind, "same key, different conclusion must be reported as changed")
	require.Equal(t, "host1", d.Key)
	require.Equal(t, "host1 critical", d.Previous)
}

func TestIdentityIgnoresNothingButContent(t *testing.T) {
	// Two stores (simulating two channels/runs) must derive the same
	// identity for the same raw message: identity contains no time,
	// channel name or formatting.
	s1 := openTemp(t, Options{})
	s1.now = func() time.Time { return time.Unix(1000, 0) }
	s2 := openTemp(t, Options{})
	s2.now = func() time.Time { return time.Unix(9999, 0) }
	require.Equal(t, s1.keyOf("a b c"), s2.keyOf("a b c"))
}

func TestPendingChannelsAndMark(t *testing.T) {
	s := openTemp(t, Options{})
	all := []string{"slack", "discord"}
	msg := "host1 open:80"

	s.Check(msg)
	require.Equal(t, all, s.PendingChannels(msg, all))

	s.Mark(msg, "slack")
	require.Equal(t, []string{"discord"}, s.PendingChannels(msg, all))

	s.Mark(msg, "discord")
	require.Empty(t, s.PendingChannels(msg, all))

	// Marking twice is idempotent.
	s.Mark(msg, "slack")
	require.Empty(t, s.PendingChannels(msg, all))
}

func TestContentChangeResetsChannels(t *testing.T) {
	s := openTemp(t, Options{KeyRegex: `^(\S+)`})
	all := []string{"slack", "discord"}

	s.Check("host1 critical")
	s.Mark("host1 critical", "slack")
	s.Mark("host1 critical", "discord")
	require.Empty(t, s.PendingChannels("host1 critical", all))

	require.Equal(t, ChangeContent, s.Check("host1 high").Kind)
	require.Equal(t, all, s.PendingChannels("host1 high", all),
		"changed content must be re-pushed to every channel")
}

func TestRetentionAllowsRepush(t *testing.T) {
	s := openTemp(t, Options{Retention: time.Hour})
	now := time.Unix(10000, 0)
	s.now = func() time.Time { return now }

	s.Check("host1 open:80")
	s.Mark("host1 open:80", "slack")

	// Within retention: suppressed.
	s.now = func() time.Time { return now.Add(30 * time.Minute) }
	require.Equal(t, ChangeNone, s.Check("host1 open:80").Kind)

	// Past retention: entry may be pushed again.
	s.now = func() time.Time { return now.Add(2 * time.Hour) }
	require.Equal(t, ChangeNew, s.Check("host1 open:80").Kind)
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := openTemp(t, Options{Path: path})
	s.Check("host1 open:80")
	s.Mark("host1 open:80", "slack")
	require.NoError(t, s.Save())

	s2, err := Open(Options{Path: path})
	require.NoError(t, err)
	require.Equal(t, ChangeNone, s2.Check("host1 open:80").Kind)
	require.Equal(t, []string{"discord"}, s2.PendingChannels("host1 open:80", []string{"slack", "discord"}))
}

func TestSaveIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := openTemp(t, Options{Path: path})
	s.Check("a")
	require.NoError(t, s.Save())
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	s.Check("b")
	require.NoError(t, s.Save())
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotEqual(t, before, after)

	// No temp files may be left behind.
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".dedup-*.tmp"))
	require.NoError(t, err)
	require.Empty(t, matches)
}

func TestCorruptStateQuarantinedAndFullPush(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

	s, err := Open(Options{Path: path})
	require.Error(t, err, "corrupt state must produce an explicit warning")
	require.Contains(t, err.Error(), "corrupt")

	// Original file quarantined, store empty -> everything is new.
	matches, gerr := filepath.Glob(path + ".corrupt-*")
	require.NoError(t, gerr)
	require.Len(t, matches, 1)
	require.Equal(t, ChangeNew, s.Check("host1 open:80").Kind)
}

func TestRemovedAndDrop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := openTemp(t, Options{Path: path})
	s.Check("host1 open:80")
	s.Check("host2 open:443")
	require.NoError(t, s.Save())

	s2, err := Open(Options{Path: path})
	require.NoError(t, err)
	s2.Check("host1 open:80") // host2 disappears from the batch

	removed := s2.Removed()
	require.Len(t, removed, 1)
	require.Equal(t, "host2 open:443", removed[0].Content)

	s2.Drop(removed[0].Key)
	require.Empty(t, s2.Removed())
}

func TestUnseenEntriesPrunedAfterRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := openTemp(t, Options{Path: path, Retention: time.Hour})
	now := time.Unix(10000, 0)
	s.now = func() time.Time { return now }
	s.Check("old-entry")
	require.NoError(t, s.Save())

	s2, err := Open(Options{Path: path, Retention: time.Hour})
	require.NoError(t, err)
	s2.now = func() time.Time { return now.Add(2 * time.Hour) }
	require.NoError(t, s2.Save())

	s3, err := Open(Options{Path: path, Retention: time.Hour})
	require.NoError(t, err)
	require.Equal(t, ChangeNew, s3.Check("old-entry").Kind,
		"entries not seen for longer than the retention are pruned")
}
