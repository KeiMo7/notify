// Package dedup implements cross-run, per-channel deduplication of notify
// messages. Identity is derived from the raw message content only (never
// from push time, channel name or provider-specific formatting), state is
// persisted as a single JSON document written atomically (temp file +
// rename), and entries expire after a configurable retention period.
package dedup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// DefaultRetention is how long an entry suppresses re-pushes before it is
// allowed to be pushed again.
const DefaultRetention = 7 * 24 * time.Hour

// maxStoredContent bounds how much of a message is kept in the state file
// so that large result sets do not bloat it.
const maxStoredContent = 4096

// ChangeKind describes why a message is eligible for pushing.
type ChangeKind int

const (
	// ChangeNone means the message was already pushed to all channels and
	// is still within its retention period.
	ChangeNone ChangeKind = iota
	// ChangeNew means the message identity has never been seen (or its
	// retention expired and it was pruned).
	ChangeNew
	// ChangeContent means the identity is known but the content hash
	// differs from the stored one.
	ChangeContent
)

func (k ChangeKind) String() string {
	switch k {
	case ChangeNew:
		return "new"
	case ChangeContent:
		return "changed"
	default:
		return "none"
	}
}

// Entry is the dedup record for one message identity.
type Entry struct {
	// Key is the original (unhashed) identity key, kept for debugging.
	Key string `json:"key"`
	// Hash is the content hash of the full message.
	Hash string `json:"hash"`
	// Content is the raw message (truncated), used to render change
	// notifications and removed-entry reports.
	Content string `json:"content"`
	// Channels lists the channels that already received the current Hash.
	Channels []string `json:"channels"`
	// FirstSeen is when this identity was first observed.
	FirstSeen time.Time `json:"first_seen"`
	// LastSeen is when this identity was last observed in an input batch.
	LastSeen time.Time `json:"last_seen"`
	// LastPushed is the most recent time any channel recorded a success.
	LastPushed time.Time `json:"last_pushed"`
}

// fileState is the on-disk document.
type fileState struct {
	Version int              `json:"version"`
	Entries map[string]Entry `json:"entries"`
}

// Decision is the outcome of checking one message.
type Decision struct {
	Kind     ChangeKind
	Key      string
	Previous string // previous content, set only for ChangeContent
}

// Store tracks pushed messages. It is not safe for concurrent use; notify
// processes messages from a single goroutine.
type Store struct {
	path      string
	retention time.Duration
	keyRe     *regexp.Regexp
	entries   map[string]*Entry
	seen      map[string]bool // identities observed in the current run
	now       func() time.Time
	dirty     bool
}

// Options configures a Store.
type Options struct {
	// Path is the state file location.
	Path string
	// Retention is how long entries suppress re-pushes. Zero uses
	// DefaultRetention.
	Retention time.Duration
	// KeyRegex optionally extracts the stable identity key from a message;
	// the first capture group is used. Empty means the whole message is
	// the identity.
	KeyRegex string
}

// Open loads (or initializes) the store at opts.Path. A missing file is not
// an error. A corrupt file is quarantined next to the original and the
// store restarts empty so the run degrades to a full push; the returned
// error is non-nil in that case and describes the recovery action.
func Open(opts Options) (*Store, error) {
	retention := opts.Retention
	if retention <= 0 {
		retention = DefaultRetention
	}
	s := &Store{
		path:      opts.Path,
		retention: retention,
		entries:   make(map[string]*Entry),
		seen:      make(map[string]bool),
		now:       time.Now,
	}
	if opts.KeyRegex != "" {
		re, err := regexp.Compile(opts.KeyRegex)
		if err != nil {
			return nil, fmt.Errorf("invalid dedup key regex: %w", err)
		}
		s.keyRe = re
	}

	data, err := os.ReadFile(opts.Path)
	switch {
	case err == nil:
		var fs fileState
		if uerr := json.Unmarshal(data, &fs); uerr != nil || fs.Entries == nil && len(data) > 0 && fs.Version == 0 {
			quarantine := fmt.Sprintf("%s.corrupt-%d", opts.Path, s.now().Unix())
			_ = os.Rename(opts.Path, quarantine)
			return s, fmt.Errorf("dedup state file %s is corrupt; moved to %s and starting fresh (this run pushes everything). Restore the quarantined file or delete it to silence this warning", opts.Path, quarantine)
		}
		for k, e := range fs.Entries {
			entry := e
			s.entries[k] = &entry
		}
	case os.IsNotExist(err):
		// first run: empty store
	default:
		return s, fmt.Errorf("could not read dedup state file %s: %w (continuing without prior state; this run pushes everything)", opts.Path, err)
	}
	return s, nil
}

// keyOf extracts the stable identity key for a message.
func (s *Store) keyOf(msg string) string {
	if s.keyRe != nil {
		if m := s.keyRe.FindStringSubmatch(msg); len(m) > 1 {
			return m[1]
		}
	}
	return msg
}

func hashOf(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func truncate(msg string) string {
	if len(msg) > maxStoredContent {
		return msg[:maxStoredContent]
	}
	return msg
}

// Check reports whether msg is new, changed, or already fully pushed.
// It records the message as seen in the current run and, for new or
// changed messages, stages the entry so that Mark can record per-channel
// successes.
func (s *Store) Check(msg string) Decision {
	key := s.keyOf(msg)
	id := hashOf(key)
	contentHash := hashOf(msg)
	s.seen[id] = true

	e, ok := s.entries[id]
	if ok && s.expired(e) {
		delete(s.entries, id)
		ok = false
	}
	if !ok {
		s.entries[id] = &Entry{
			Key:       key,
			Hash:      contentHash,
			Content:   truncate(msg),
			FirstSeen: s.now(),
			LastSeen:  s.now(),
		}
		s.dirty = true
		return Decision{Kind: ChangeNew, Key: key}
	}
	e.LastSeen = s.now()
	if e.Hash != contentHash {
		prev := e.Content
		e.Hash = contentHash
		e.Content = truncate(msg)
		e.Channels = nil // content changed: every channel needs the update
		s.dirty = true
		return Decision{Kind: ChangeContent, Key: key, Previous: prev}
	}
	return Decision{Kind: ChangeNone, Key: key}
}

// expired reports whether the entry's last successful push is older than
// the retention period.
func (s *Store) expired(e *Entry) bool {
	ref := e.LastPushed
	if ref.IsZero() {
		ref = e.LastSeen
	}
	return s.now().Sub(ref) > s.retention
}

// PendingChannels returns the subset of channels that have not yet
// recorded a successful push for the message's current content.
func (s *Store) PendingChannels(msg string, channels []string) []string {
	id := hashOf(s.keyOf(msg))
	e, ok := s.entries[id]
	if !ok {
		return channels
	}
	if s.expired(e) {
		return channels
	}
	done := make(map[string]bool, len(e.Channels))
	for _, c := range e.Channels {
		done[c] = true
	}
	var pending []string
	for _, c := range channels {
		if !done[c] {
			pending = append(pending, c)
		}
	}
	return pending
}

// Mark records a successful push of msg to channel. It must only be called
// after the channel confirmed success; failures must not be marked.
func (s *Store) Mark(msg, channel string) {
	id := hashOf(s.keyOf(msg))
	e, ok := s.entries[id]
	if !ok {
		return
	}
	for _, c := range e.Channels {
		if c == channel {
			return
		}
	}
	e.Channels = append(e.Channels, channel)
	sort.Strings(e.Channels)
	e.LastPushed = s.now()
	s.dirty = true
}

// Removed returns the stored contents of entries that were not observed in
// the current run, i.e. results that disappeared since the previous push.
func (s *Store) Removed() []Entry {
	var out []Entry
	for id, e := range s.entries {
		if !s.seen[id] {
			out = append(out, *e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Drop removes an entry (used after a removed-entry report was pushed).
func (s *Store) Drop(key string) {
	id := hashOf(key)
	if _, ok := s.entries[id]; ok {
		delete(s.entries, id)
		s.dirty = true
	}
}

// Save persists the store atomically: entries not seen within the
// retention window are pruned, the document is written to a temp file in
// the same directory, fsynced and renamed over the target, so a kill
// mid-write never leaves a half-written state file.
func (s *Store) Save() error {
	now := s.now()
	for id, e := range s.entries {
		if now.Sub(e.LastSeen) > s.retention {
			delete(s.entries, id)
			s.dirty = true
		}
	}
	fs := fileState{Version: 1, Entries: make(map[string]Entry, len(s.entries))}
	for id, e := range s.entries {
		fs.Entries[id] = *e
	}
	data, err := json.MarshalIndent(fs, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("could not create dedup state directory %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".dedup-*.tmp")
	if err != nil {
		return fmt.Errorf("could not create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	s.dirty = false
	return nil
}
