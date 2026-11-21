package reliability

import (
	"testing"
)

// Benchmark measures the steady-state overhead of the enabled reliability
// path when every send succeeds (the common case). The work compared against
// is the same adapter call with the feature absent.
type benchSender struct{}

func (benchSender) Send(_, _ string) error { return nil }

func BenchmarkReliableSendSuccess(b *testing.B) {
	cfg := Config{
		Enabled: true, MaxRetries: 3, BackoffInitial: 1, BackoffMax: 1,
		MaxRetryTime: 1e9, FailureThreshold: 5, Cooldown: 1e9,
	}
	p := NewReliableProvider("bench", benchSender{}, cfg, &signalCollector{})
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := p.Send("message", ""); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLegacySend is the pre-change equivalent: a bare adapter call with
// no breaker, collector or classifier. The delta between this and
// BenchmarkReliableSendSuccess is the per-message cost of the enabled feature.
func BenchmarkLegacySend(b *testing.B) {
	sender := benchSender{}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := sender.Send("message", ""); err != nil {
			b.Fatal(err)
		}
	}
}

// Deduper hot path: a fresh message each iteration so the map stays small.
func BenchmarkDeduperAdmitFresh(b *testing.B) {
	d := NewDeduper(0) // window zero: the cheapest possible (disabled) path
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		d.Admit("message")
	}
}

func BenchmarkDeduperAdmitWindowed(b *testing.B) {
	d := NewDeduper(0)
	d.window = 1 << 62 // effectively never expires
	b.ReportAllocs()
	b.Run("first", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			d.Admit("constant-message")
		}
	})
}
