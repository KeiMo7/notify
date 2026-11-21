package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/projectdiscovery/notify/pkg/dedup"
	"github.com/projectdiscovery/notify/pkg/providers"
	"github.com/projectdiscovery/notify/pkg/types"
)

// fakeChannels is a providerClient with two channels ("slack", "discord")
// that records every message per channel and can be told to fail.
type fakeChannels struct {
	mu       sync.Mutex
	failures map[string]bool
	sent     map[string][]string
}

func newFakeChannels() *fakeChannels {
	return &fakeChannels{
		failures: make(map[string]bool),
		sent:     map[string][]string{"slack": {}, "discord": {}},
	}
}

func (f *fakeChannels) Names() []string { return []string{"slack", "discord"} }

func (f *fakeChannels) Send(message string) error {
	for _, res := range f.SendWithResults(message, nil) {
		if res.Err != nil {
			return res.Err
		}
	}
	return nil
}

func (f *fakeChannels) SendWithResults(message string, only []string) []providers.SendResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	filter := make(map[string]bool, len(only))
	for _, name := range only {
		filter[name] = true
	}
	var results []providers.SendResult
	for _, name := range f.Names() {
		if len(filter) > 0 && !filter[name] {
			continue
		}
		if f.failures[name] {
			results = append(results, providers.SendResult{Provider: name, Err: fmt.Errorf("%s: simulated send failure", name)})
			continue
		}
		f.sent[name] = append(f.sent[name], message)
		results = append(results, providers.SendResult{Provider: name})
	}
	return results
}

func (f *fakeChannels) received(channel string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent[channel]...)
}

func newDedupRunner(t *testing.T, storePath string, client providerClient, mutate func(*types.Options)) *Runner {
	t.Helper()
	options := &types.Options{Dedup: true, DedupStore: storePath}
	if mutate != nil {
		mutate(options)
	}
	store, err := dedup.Open(dedup.Options{
		Path:     storePath,
		KeyRegex: options.DedupKeyRegex,
	})
	require.NoError(t, err)
	return &Runner{options: options, providers: client, deduper: store}
}

// Case 1: dedup disabled — every message is pushed, every time.
func TestDedupDisabledPushesEverything(t *testing.T) {
	fake := newFakeChannels()
	r := &Runner{options: &types.Options{}, providers: fake}

	require.NoError(t, r.sendMessage("host1 open:80"))
	require.NoError(t, r.sendMessage("host1 open:80"))
	require.Equal(t, []string{"host1 open:80", "host1 open:80"}, fake.received("slack"))
	require.Equal(t, []string{"host1 open:80", "host1 open:80"}, fake.received("discord"))
}

// Case 2: dedup enabled — a repeated batch only pushes the new entries.
func TestDedupOnlyPushesNewEntries(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "dedup.json")
	fake := newFakeChannels()

	r1 := newDedupRunner(t, storePath, fake, nil)
	require.NoError(t, r1.sendMessage("host1 open:80"))
	require.NoError(t, r1.sendMessage("host2 open:443"))
	require.NoError(t, r1.saveDedup())
	require.Equal(t, []string{"host1 open:80", "host2 open:443"}, fake.received("slack"))

	// Second run: same batch plus one new entry.
	r2 := newDedupRunner(t, storePath, fake, nil)
	require.NoError(t, r2.sendMessage("host1 open:80"))
	require.NoError(t, r2.sendMessage("host2 open:443"))
	require.NoError(t, r2.sendMessage("host3 open:22"))
	require.NoError(t, r2.saveDedup())

	require.Equal(t, []string{"host1 open:80", "host2 open:443", "host3 open:22"}, fake.received("slack"),
		"each channel must receive each result exactly once across runs")
	require.Equal(t, []string{"host1 open:80", "host2 open:443", "host3 open:22"}, fake.received("discord"))
}

// Case 3: same identity, changed conclusion — pushed with a change marker.
func TestDedupPushesChangedContent(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "dedup.json")
	fake := newFakeChannels()
	withKey := func(o *types.Options) { o.DedupKeyRegex = `^(\S+)` }

	r1 := newDedupRunner(t, storePath, fake, withKey)
	require.NoError(t, r1.sendMessage("host1 severity:critical"))
	require.NoError(t, r1.saveDedup())

	r2 := newDedupRunner(t, storePath, fake, withKey)
	require.NoError(t, r2.sendMessage("host1 severity:high"))
	require.NoError(t, r2.saveDedup())

	got := fake.received("slack")
	require.Len(t, got, 2)
	require.Contains(t, got[1], "[notify:changed] host1")
	require.Contains(t, got[1], "- host1 severity:critical")
	require.Contains(t, got[1], "+ host1 severity:high")
}

// Case 4: one channel fails — failed entries are NOT marked as pushed and
// are retried on the next run, while the healthy channel is not spammed.
func TestDedupPartialChannelFailure(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "dedup.json")
	fake := newFakeChannels()
	fake.failures["discord"] = true

	r1 := newDedupRunner(t, storePath, fake, nil)
	require.Error(t, r1.sendMessage("host1 open:80"))
	require.NoError(t, r1.saveDedup())

	// The state file must record only the successful channel.
	data, err := os.ReadFile(storePath)
	require.NoError(t, err)
	var state struct {
		Entries map[string]struct {
			Channels []string `json:"channels"`
		} `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(data, &state))
	require.Len(t, state.Entries, 1)
	for _, e := range state.Entries {
		require.Equal(t, []string{"slack"}, e.Channels,
			"failed channel must not be marked as pushed")
	}

	// Next run: discord recovers. Only discord gets the message; slack
	// must not receive it a second time.
	fake.failures["discord"] = false
	r2 := newDedupRunner(t, storePath, fake, nil)
	require.NoError(t, r2.sendMessage("host1 open:80"))
	require.NoError(t, r2.saveDedup())

	require.Equal(t, []string{"host1 open:80"}, fake.received("slack"),
		"slack already received the result and must not be re-notified")
	require.Equal(t, []string{"host1 open:80"}, fake.received("discord"),
		"discord receives the result exactly once, after recovery")
}

// Removed entries are reported only when the flag is set.
func TestDedupReportRemoved(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "dedup.json")
	fake := newFakeChannels()

	r1 := newDedupRunner(t, storePath, fake, nil)
	require.NoError(t, r1.sendMessage("host1 open:80"))
	require.NoError(t, r1.sendMessage("host2 open:443"))
	require.NoError(t, r1.saveDedup())

	// Default: removed entries are not reported (Run only calls
	// reportRemoved when DedupReportRemoved is set).
	r2 := newDedupRunner(t, storePath, fake, nil)
	require.NoError(t, r2.sendMessage("host1 open:80"))
	require.NoError(t, r2.saveDedup())
	require.Equal(t, []string{"host1 open:80", "host2 open:443"}, fake.received("slack"),
		"nothing new may be pushed for a disappeared entry by default")

	// Opt-in: the disappeared entry is pushed once and dropped.
	r3 := newDedupRunner(t, storePath, fake, func(o *types.Options) { o.DedupReportRemoved = true })
	require.NoError(t, r3.sendMessage("host1 open:80"))
	r3.reportRemoved()
	require.NoError(t, r3.saveDedup())
	got := fake.received("slack")
	require.Len(t, got, 3)
	require.Contains(t, got[2], "[notify:removed] host2 open:443")

	r4 := newDedupRunner(t, storePath, fake, func(o *types.Options) { o.DedupReportRemoved = true })
	require.NoError(t, r4.sendMessage("host1 open:80"))
	r4.reportRemoved()
	require.Equal(t, got, fake.received("slack"), "removal must be reported only once")
}

// Corrupt state file: warn and fall back to a full push.
func TestDedupCorruptStateFallsBackToFullPush(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "dedup.json")
	require.NoError(t, os.WriteFile(storePath, []byte("###garbage###"), 0o600))

	// Open quarantines the corrupt file and returns an empty store plus a
	// warning error; the runner logs it and pushes everything.
	store, err := dedup.Open(dedup.Options{Path: storePath})
	require.Error(t, err)
	require.Contains(t, err.Error(), "corrupt")
	fake := newFakeChannels()
	r := &Runner{options: &types.Options{Dedup: true, DedupStore: storePath}, providers: fake, deduper: store}
	require.NoError(t, r.sendMessage("host1 open:80"))
	require.Equal(t, []string{"host1 open:80"}, fake.received("slack"))
}

// Unwritable state directory: warn and keep pushing without dedup.
func TestDedupUnwritableStoreDegrades(t *testing.T) {
	// A regular file used as a directory makes MkdirAll/CreateTemp fail.
	blocker := filepath.Join(t.TempDir(), "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))
	storePath := filepath.Join(blocker, "dedup.json")

	// Open reports the unreadable location as a warning error but still
	// returns a usable (empty) store; the Save probe then fails and the
	// runner degrades to no dedup (mirrors NewRunner).
	store, err := dedup.Open(dedup.Options{Path: storePath})
	require.Error(t, err)
	require.Error(t, store.Save(), "unwritable location must surface an error")

	// Runner degrades to no dedup (mirrors NewRunner's probe).
	fake := newFakeChannels()
	r := &Runner{options: &types.Options{}, providers: fake}
	require.NoError(t, r.sendMessage("host1 open:80"))
	require.Equal(t, []string{"host1 open:80"}, fake.received("slack"))
}
