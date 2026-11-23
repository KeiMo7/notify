package message

import (
	"strings"
	"testing"
)

func TestRedactURLEmbeddedCredentials(t *testing.T) {
	in := "connect to https://admin:hunter2@db.internal:5432/secret now"
	out := Redact(in)
	for _, leak := range []string{"hunter2", "admin", "db.internal", "5432"} {
		if strings.Contains(out, leak) {
			t.Fatalf("cleartext %q survived redaction: %q", leak, out)
		}
	}
	if !strings.Contains(out, RedactedMarker) {
		t.Fatalf("redaction marker missing: %q", out)
	}
	// The scheme survives so the fragment still reads as a URL.
	if !strings.Contains(out, "https://") {
		t.Fatalf("scheme should be preserved: %q", out)
	}
}

func TestRedactSecretAssignments(t *testing.T) {
	cases := map[string]string{
		"token=abcdef1234567890verysecretvalue": "abcdef1234567890",
		`api_key: "AKIA1234567890SECRET"`:       "AKIA1234567890SECRET",
		"password='p@ss word!'":                 "p@ss",
		"Authorization: Bearer abc.def.ghi":     "abc.def.ghi",
	}
	for in, leak := range cases {
		out := Redact(in)
		if strings.Contains(out, leak) {
			t.Fatalf("secret %q leaked from %q -> %q", leak, in, out)
		}
		if !strings.Contains(out, RedactedMarker) {
			t.Fatalf("no marker for %q -> %q", in, out)
		}
	}
}

func TestRedactPrivateIPs(t *testing.T) {
	cases := []string{
		"host 192.168.1.10 unreachable",
		"dial 10.0.0.5:8080 timeout",
		"loopback 127.0.0.1 refused",
		"linklocal 169.254.1.1 down",
	}
	for _, in := range cases {
		out := Redact(in)
		if strings.Contains(out, "192.168") || strings.Contains(out, "10.0.0") ||
			strings.Contains(out, "127.0.0") || strings.Contains(out, "169.254") {
			t.Fatalf("internal address leaked from %q -> %q", in, out)
		}
	}
}

func TestRedactInternalHostnames(t *testing.T) {
	cases := []string{
		"connect to db.internal:5432 now",
		"see http://service.local/path for details",
		"call https://host.corp/api/x?y=1",
		"dev server on localhost:3000",
		"bare build.agent01.lan reachable",
	}
	for _, in := range cases {
		out := Redact(in)
		for _, leak := range []string{"db.internal", "service.local", "host.corp", "localhost", "agent01.lan", ":5432", ":3000"} {
			if strings.Contains(out, leak) {
				t.Fatalf("internal hostname %q leaked from %q -> %q", leak, in, out)
			}
		}
		if !strings.Contains(out, RedactedMarker) {
			t.Fatalf("no marker for %q -> %q", in, out)
		}
	}
}

func TestRedactKeepsPublicAddresses(t *testing.T) {
	in := "see https://example.com/path and 8.8.8.8 for details"
	out := Redact(in)
	if !strings.Contains(out, "example.com") || !strings.Contains(out, "8.8.8.8") {
		t.Fatalf("public addresses must be preserved: %q", out)
	}
}

func TestRedactIsIdempotentAndUniform(t *testing.T) {
	in := "token=zzzz1234 at 10.1.2.3"
	once := Redact(in)
	twice := Redact(once)
	if once != twice {
		t.Fatalf("redaction must be stable/idempotent:\n once=%q\ntwice=%q", once, twice)
	}
}

func TestRedactDoesNotChangeNonSecretSemantics(t *testing.T) {
	in := "CPU 92% on node-7; disk 8% free; status=ok"
	if got := Redact(in); got != in {
		t.Fatalf("innocuous content altered: %q", got)
	}
}
