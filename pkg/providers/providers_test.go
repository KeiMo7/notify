package providers

import (
	"errors"
	"strings"
	"testing"

	msgpkg "github.com/projectdiscovery/notify/pkg/message"
	"github.com/projectdiscovery/notify/pkg/types"
)

// fakeChannel records every rendered fragment it is asked to deliver and
// can be configured to fail, exercising the channel-isolation path.
type fakeChannel struct {
	name         string
	budget       int
	fail         bool
	got          []string
	prepareCalls int
}

func (f *fakeChannel) Name() string { return f.name }

func (f *fakeChannel) Validate() error {
	return msgpkg.ValidateBudget(f.budget)
}

func (f *fakeChannel) Prepare(redacted string) ([]msgpkg.Chunk, error) {
	f.prepareCalls++
	return msgpkg.Process(redacted, msgpkg.Profile{Budget: f.budget})
}

func (f *fakeChannel) SendChunks(chunks []msgpkg.Chunk, _ string) error {
	if f.fail {
		return errors.New("simulated channel outage")
	}
	for _, c := range chunks {
		f.got = append(f.got, msgpkg.RenderChunk(c))
	}
	return nil
}

func TestNewFromClientsRejectsIllegalBudgetAtStartup(t *testing.T) {
	bad := &fakeChannel{name: "broken", budget: 5}
	if _, err := NewFromClients([]ChunkProvider{bad}, &types.Options{}); err == nil {
		t.Fatal("illegal budget must be rejected during client construction")
	}
}

func TestSendRedactsUniformlyAcrossChannels(t *testing.T) {
	a := &fakeChannel{name: "a", budget: 200}
	b := &fakeChannel{name: "b", budget: 80}
	client, err := NewFromClients([]ChunkProvider{a, b}, &types.Options{})
	if err != nil {
		t.Fatal(err)
	}

	msg := "login anomaly token=abcdef1234567890secretvalue at 10.20.30.40\n" +
		"see https://svc:pw@db.internal:5432/x"
	if err := client.Send(msg); err != nil {
		t.Fatalf("Send must not return an error when a channel is healthy: %v", err)
	}

	secrets := []string{"abcdef1234567890", "10.20.30.40", "db.internal", "svc:pw", "5432"}
	for _, f := range []*fakeChannel{a, b} {
		if len(f.got) == 0 {
			t.Fatalf("channel %s received nothing", f.name)
		}
		joined := strings.Join(f.got, "\n")
		for _, leak := range secrets {
			if strings.Contains(joined, leak) {
				t.Fatalf("channel %s leaked cleartext %q: %q", f.name, leak, joined)
			}
		}
		if !strings.Contains(joined, msgpkg.RedactedMarker) {
			t.Fatalf("channel %s never saw a redaction marker", f.name)
		}
	}
}

func TestSendIsolatesChannelFailures(t *testing.T) {
	dead := &fakeChannel{name: "dead", budget: 80, fail: true}
	alive := &fakeChannel{name: "alive", budget: 80}
	client, err := NewFromClients([]ChunkProvider{dead, alive}, &types.Options{})
	if err != nil {
		t.Fatal(err)
	}

	// Must not return an error: the healthy channel still gets its full set.
	if err := client.Send(strings.Repeat("隔离行 ", 60)); err != nil {
		t.Fatalf("a failing channel must not make Send return an error: %v", err)
	}
	if len(alive.got) == 0 {
		t.Fatal("healthy channel received nothing after the other failed")
	}
	if len(dead.got) != 0 {
		t.Fatal("failing channel must not have recorded deliveries")
	}
}

func TestSendDropsBlankMessages(t *testing.T) {
	f := &fakeChannel{name: "f", budget: 100}
	client, err := NewFromClients([]ChunkProvider{f}, &types.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, blank := range []string{"", "   ", "\n\t  \n"} {
		if err := client.Send(blank); err != nil {
			t.Fatalf("blank message returned error: %v", err)
		}
	}
	if len(f.got) != 0 {
		t.Fatalf("blank messages must not be delivered, got %v", f.got)
	}
}

func TestDifferentBudgetsProduceDifferentChunkCounts(t *testing.T) {
	small := &fakeChannel{name: "small", budget: 60}
	large := &fakeChannel{name: "large", budget: 300}
	client, err := NewFromClients([]ChunkProvider{small, large}, &types.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Send(strings.Repeat("告警数据😀 ", 60)); err != nil {
		t.Fatal(err)
	}
	if len(small.got) <= len(large.got) {
		t.Fatalf("smaller budget must yield more chunks: small=%d large=%d", len(small.got), len(large.got))
	}
	// Every fragment respects its own budget including the header.
	for _, c := range small.got {
		if msgpkg.RuneLen(c) > 60 {
			t.Fatalf("small-channel fragment over budget: %d", msgpkg.RuneLen(c))
		}
	}
}
