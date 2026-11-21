package reliability

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// QueueEntry is one message persisted while every provider was unavailable.
type QueueEntry struct {
	EnqueuedAt string `json:"enqueued_at"`
	Message    string `json:"message"`
}

// LocalQueue is a bounded, file-backed FIFO spill queue. The whole state is
// rewritten atomically (temp file + rename, fsync) after every mutation:
// throughput here is irrelevant, durability across a crash is what matters.
// When the bound is reached the oldest entries are dropped and counted, so
// the file cannot grow without limit.
type LocalQueue struct {
	path string
	max  int

	mu      sync.Mutex
	entries []QueueEntry
	dropped int64
}

// queueFile is the on-disk representation; keeping the counters next to the
// entries makes a crash and restart preserve the drop accounting.
type queueFile struct {
	Dropped int64        `json:"dropped"`
	Entries []QueueEntry `json:"entries"`
}

// OpenLocalQueue loads an existing queue from path (creating nothing until
// the first enqueue). A corrupt file is quarantined rather than silently
// discarded: it is renamed to <path>.corrupt-<unix-nanos> and processing
// continues with an empty queue, because malformed upstream/local data must
// never take the send path down.
func OpenLocalQueue(path string, max int) (*LocalQueue, error) {
	q := &LocalQueue{path: path, max: max}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return q, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return q, nil
	}

	var onDisk queueFile
	if err := json.Unmarshal(data, &onDisk); err != nil {
		quarantine := path + ".corrupt"
		if renameErr := os.Rename(path, quarantine); renameErr != nil {
			return nil, renameErr
		}
		return q, nil
	}
	q.entries = onDisk.Entries
	q.dropped = onDisk.Dropped
	q.applyCapLocked()
	return q, nil
}

// Enqueue appends one entry, evicting the oldest when the bound is exceeded.
// The eviction count is part of the persisted state.
func (q *LocalQueue) Enqueue(entry QueueEntry) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.entries = append(q.entries, entry)
	q.applyCapLocked()
	return q.persistLocked()
}

// applyCapLocked trims oldest entries beyond the bound and counts each drop.
func (q *LocalQueue) applyCapLocked() {
	if q.max <= 0 {
		return
	}
	if excess := len(q.entries) - q.max; excess > 0 {
		q.dropped += int64(excess)
		q.entries = q.entries[excess:]
	}
}

// Drain removes and returns all entries in FIFO order. Persistence happens
// after the caller's replay succeeds via Acknowledge; a crash in between
// leaves the file untouched, so the same entries are replayed on the next
// start instead of being delivered twice-then-lost.
func (q *LocalQueue) Drain() []QueueEntry {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.entries) == 0 {
		return nil
	}
	out := make([]QueueEntry, len(q.entries))
	copy(out, q.entries)
	return out
}

// Acknowledge clears entries that were replayed successfully and persists.
func (q *LocalQueue) Acknowledge(processed int) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if processed >= len(q.entries) {
		q.entries = nil
	} else {
		q.entries = q.entries[processed:]
	}
	return q.persistLocked()
}

// Len returns the current queue length.
func (q *LocalQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.entries)
}

// Dropped returns how many oldest entries have been evicted since the queue
// was created, including counts restored from disk.
func (q *LocalQueue) Dropped() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.dropped
}

// Path reports where the queue is persisted.
func (q *LocalQueue) Path() string { return q.path }

// persistLocked writes the state atomically. The caller holds q.mu.
func (q *LocalQueue) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(q.path), 0o755); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(queueFile{Dropped: q.dropped, Entries: q.entries}, "", "  ")
	if err != nil {
		return err
	}

	tmp := q.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	if _, err := w.Write(payload); err != nil {
		_ = f.Close()
		return err
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, q.path)
}
