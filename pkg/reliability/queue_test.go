package reliability

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Scenario 4: when every provider is unavailable the message lands in the
// local queue and survives a restart; nothing is silently discarded.
func TestLocalQueuePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.json")

	q, err := OpenLocalQueue(path, 10)
	require.NoError(t, err)
	require.NoError(t, q.Enqueue(QueueEntry{EnqueuedAt: "t1", Message: "first"}))
	require.NoError(t, q.Enqueue(QueueEntry{EnqueuedAt: "t2", Message: "second"}))

	reopened, err := OpenLocalQueue(path, 10)
	require.NoError(t, err)
	entries := reopened.Drain()
	require.Len(t, entries, 2)
	require.Equal(t, "first", entries[0].Message) // FIFO order
	require.Equal(t, "second", entries[1].Message)

	// Acknowledge one; the other must remain on disk after reopen.
	require.NoError(t, reopened.Acknowledge(1))
	reopened2, err := OpenLocalQueue(path, 10)
	require.NoError(t, err)
	require.Len(t, reopened2.Drain(), 1)
}

// Backlog cap: overflow drops the OLDEST entries and counts them; the file
// can never grow beyond the bound.
func TestLocalQueueCapDropsOldestAndCounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.json")
	q, err := OpenLocalQueue(path, 3)
	require.NoError(t, err)

	for i := 0; i < 5; i++ {
		require.NoError(t, q.Enqueue(QueueEntry{Message: string(rune('a' + i))}))
	}
	require.Equal(t, int64(2), q.Dropped())
	entries := q.Drain()
	require.Len(t, entries, 3)
	require.Equal(t, "c", entries[0].Message, "oldest two ('a','b') must be evicted")
	require.Equal(t, "e", entries[2].Message)

	// Drop accounting persists too.
	reopened, err := OpenLocalQueue(path, 3)
	require.NoError(t, err)
	require.Equal(t, int64(2), reopened.Dropped())
}

// A corrupt queue file is quarantined, not fatal: startup continues with an
// empty queue and the bad bytes are preserved for forensics.
func TestLocalQueueCorruptFileQuarantined(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.json")
	require.NoError(t, os.WriteFile(path, []byte("{not-json"), 0o600))

	q, err := OpenLocalQueue(path, 5)
	require.NoError(t, err)
	require.Zero(t, q.Len())

	matches, err := filepath.Glob(path + ".corrupt*")
	require.NoError(t, err)
	require.Len(t, matches, 1, "corrupt file must be moved aside, not deleted")

	// New writes work normally and create a fresh valid file.
	require.NoError(t, q.Enqueue(QueueEntry{Message: "recovered"}))
	reopened, err := OpenLocalQueue(path, 5)
	require.NoError(t, err)
	require.Len(t, reopened.Drain(), 1)
}

func TestLocalQueueAcknowledgeAllClearsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.json")
	q, err := OpenLocalQueue(path, 10)
	require.NoError(t, err)
	require.NoError(t, q.Enqueue(QueueEntry{Message: "x"}))
	require.NoError(t, q.Acknowledge(10))
	require.Zero(t, q.Len())

	reopened, err := OpenLocalQueue(path, 10)
	require.NoError(t, err)
	require.Nil(t, reopened.Drain())
}
