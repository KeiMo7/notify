package runner

import (
	"bufio"
	"strings"
	"testing"
	"unicode/utf8"
)

// runSplitter drives a bufio.SplitFunc over input and returns every token.
func runSplitter(t *testing.T, split bufio.SplitFunc, input string, bufSize int) []string {
	t.Helper()
	scanner := bufio.NewScanner(strings.NewReader(input))
	scanner.Buffer(make([]byte, 0, bufSize), bufSize)
	scanner.Split(split)
	var tokens []string
	for scanner.Scan() {
		tokens = append(tokens, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}
	return tokens
}

func TestLineSplitterTruncatesOnRuneBoundary(t *testing.T) {
	// 60 multi-byte runes; byte length (180) far exceeds the char limit.
	line := strings.Repeat("中", 60)
	split, err := lineLengthSplitter(40)
	if err != nil {
		t.Fatal(err)
	}
	tokens := runSplitter(t, split, line, 256)
	if len(tokens) == 0 {
		t.Fatal("expected at least one token")
	}
	for _, tok := range tokens {
		if !utf8.ValidString(tok) {
			t.Fatalf("token is not valid UTF-8 (half rune): %x", tok)
		}
		if utf8.RuneCountInString(tok) > 40 {
			t.Fatalf("token exceeds char limit: %d runes", utf8.RuneCountInString(tok))
		}
	}
}

func TestBulkSplitterTruncatesOnRuneBoundary(t *testing.T) {
	input := strings.Repeat("告警行😀", 20) + "\n" + strings.Repeat("数据", 20)
	split, err := bulkSplitter(50)
	if err != nil {
		t.Fatal(err)
	}
	tokens := runSplitter(t, split, input, 1024)
	if len(tokens) == 0 {
		t.Fatal("expected tokens")
	}
	for _, tok := range tokens {
		if !utf8.ValidString(tok) {
			t.Fatalf("bulk token is not valid UTF-8: %x", tok)
		}
		if utf8.RuneCountInString(tok) > 50 {
			t.Fatalf("bulk token over char limit: %d", utf8.RuneCountInString(tok))
		}
	}
}

func TestSplittersRejectTinyLimit(t *testing.T) {
	if _, err := lineLengthSplitter(2); err == nil {
		t.Fatal("limit below ellipsis length must be rejected")
	}
	if _, err := bulkSplitter(2); err == nil {
		t.Fatal("limit below ellipsis length must be rejected")
	}
}

func TestBulkSplitterKeepsShortMultilineIntact(t *testing.T) {
	input := "line one\nline two\nline three"
	split, err := bulkSplitter(4000)
	if err != nil {
		t.Fatal(err)
	}
	tokens := runSplitter(t, split, input, 64*1024)
	if len(tokens) != 1 || tokens[0] != input {
		t.Fatalf("short multiline input should stay one token, got %q", tokens)
	}
}
