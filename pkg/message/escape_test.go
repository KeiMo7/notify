package message

import (
	"strings"
	"testing"
)

func TestEscapeTelegramHTML(t *testing.T) {
	in := "a < b && c > d"
	want := "a &lt; b &amp;&amp; c &gt; d"
	if got := EscapeTelegramHTML(in); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestEscapeTelegramMarkdown(t *testing.T) {
	in := "use *bold* and _italic_ with [link] and `code`"
	got := EscapeTelegramMarkdown(in)
	for _, ch := range []string{"*", "_", "[", "`"} {
		if strings.Contains(got, ch) && !strings.Contains(got, "\\"+ch) {
			t.Fatalf("unescaped %s in %q", ch, got)
		}
	}
}

func TestEscapeTelegramMarkdownV2(t *testing.T) {
	in := "hello.world! (test) | x = y"
	got := EscapeTelegramMarkdownV2(in)
	for _, ch := range []string{".", "!", "(", ")", "|", "="} {
		if !strings.Contains(got, "\\"+ch) {
			t.Fatalf("MarkdownV2 special %s not escaped: %q", ch, got)
		}
	}
}

func TestEscapeChatMarkdown(t *testing.T) {
	in := "rate < 50% > 10% & rising"
	got := EscapeChatMarkdown(in)
	if strings.Contains(got, "<") || strings.Contains(got, ">") || strings.Contains(got, "&r") {
		t.Fatalf("mrkdwn metacharacters not escaped: %q", got)
	}
}

func TestParseTelegramSyntax(t *testing.T) {
	cases := map[string]Syntax{
		"":           SyntaxNone,
		"none":       SyntaxNone,
		"HTML":       SyntaxTelegramHTML,
		"markdown":   SyntaxTelegramMarkdown,
		"MarkdownV2": SyntaxTelegramMarkdownV2,
	}
	for in, want := range cases {
		if got := ParseTelegramSyntax(in); got != want {
			t.Fatalf("ParseTelegramSyntax(%q)=%v want %v", in, got, want)
		}
	}
}

func TestApplySyntaxRoundTripDoesNotLoseContent(t *testing.T) {
	in := "告警 test 数据 重要 😀"
	for _, syntax := range []Syntax{SyntaxTelegramHTML, SyntaxTelegramMarkdown, SyntaxTelegramMarkdownV2, SyntaxChatMarkdown} {
		got := ApplySyntax(in, syntax)
		// Plain content (no markup metacharacters) must pass through every
		// escaper unchanged: escaping must not alter ordinary semantics.
		if got != in {
			t.Fatalf("syntax %v altered plain content:\n got=%q\nwant=%q", syntax, got, in)
		}
	}
}

func TestApplySyntaxEscapesMarkupButPreservesMeaning(t *testing.T) {
	in := "a < b && c > d *emphasis* _x_"
	html := ApplySyntax(in, SyntaxTelegramHTML)
	if strings.Contains(html, "< b") || strings.Contains(html, "> d") {
		t.Fatalf("HTML angle brackets not escaped: %q", html)
	}
	// Every non-markup content rune is still present.
	for _, r := range "a  b  c  d emphasis x" {
		if !strings.ContainsRune(html, r) {
			t.Fatalf("content rune %q lost under HTML escaping: %q", r, html)
		}
	}
}

func TestProcessBlankReturnsNoChunks(t *testing.T) {
	chunks, err := Process("   \n\t ", Profile{Budget: 100})
	if err != nil {
		t.Fatal(err)
	}
	if chunks != nil {
		t.Fatalf("blank message must yield nil chunks, got %v", chunks)
	}
}

func TestProcessInvalidUTF8ErrorsAndKeepsOriginal(t *testing.T) {
	raw := string([]byte{'a', 0xff, 'b'})
	chunks, err := Process(raw, Profile{Budget: 100})
	if err != ErrInvalidUTF8 {
		t.Fatalf("want ErrInvalidUTF8, got %v", err)
	}
	if chunks != nil {
		t.Fatal("no chunks expected on encoding failure")
	}
}

func TestProcessRedactsBeforeSplit(t *testing.T) {
	chunks, err := Process("token=abcdef1234567890secret "+strings.Repeat("x", 100), Profile{Budget: 40})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected splitting, got %d", len(chunks))
	}
	for _, c := range chunks {
		if strings.Contains(c.Text, "abcdef1234567890") {
			t.Fatalf("cleartext secret reached a chunk: %q", c.Text)
		}
	}
}
