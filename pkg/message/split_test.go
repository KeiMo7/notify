package message

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplitRuneBudgetNotBytes(t *testing.T) {
	// Each Chinese rune is 3 bytes; a 50-rune budget still counts runes,
	// not the 150 bytes. MinBudget (40) is the floor.
	msg := "中文测试告警消息拆分边界情况哦" + strings.Repeat("字", 60) // > 50 runes
	chunks, err := Split(msg, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 2 {
		t.Fatalf("want multiple chunks, got %d", len(chunks))
	}
	for _, c := range chunks {
		if !utf8.ValidString(c.Text) {
			t.Fatalf("invalid utf8: %q", c.Text)
		}
		if RuneLen(render(c)) > 50 {
			t.Fatalf("chunk exceeds rune budget: %d", RuneLen(render(c)))
		}
	}
	if got := join(chunks); got != msg {
		t.Fatalf("reassembly mismatch:\n got=%q\nwant=%q", got, msg)
	}
}

func TestSplitNeverCutsEmoji(t *testing.T) {
	// Breakable input: spaces give field boundaries, emoji sit inside words.
	msg := strings.Repeat("a ", 30) + "boundary😀🚀🎉word " + strings.Repeat("b ", 29) + "end"
	chunks, err := Split(msg, 40)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chunks {
		if !utf8.ValidString(c.Text) {
			t.Fatalf("emoji corrupted: %x", c.Text)
		}
	}
	joined := join(chunks)
	if !strings.Contains(joined, "😀🚀🎉") {
		t.Fatalf("emoji sequence lost: %q", joined)
	}
	// Separators may be normalized at chunk boundaries; compare the
	// whitespace-insensitive token sequence to prove no content rune lost.
	if strings.Fields(joined) == nil || !sameTokens(joined, msg) {
		t.Fatalf("content token sequence changed:\n got=%q\nwant=%q", joined, msg)
	}
}

func sameTokens(a, b string) bool {
	ta, tb := strings.Fields(a), strings.Fields(b)
	if len(ta) != len(tb) {
		return false
	}
	for i := range ta {
		if ta[i] != tb[i] {
			return false
		}
	}
	return true
}

func TestSplitPrefersLineBoundary(t *testing.T) {
	line1 := strings.Repeat("x", 30)
	line2 := strings.Repeat("y", 30)
	msg := line1 + "\n" + line2
	chunks, err := Split(msg, 45)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 {
		t.Fatalf("want 2 chunks split at newline, got %d", len(chunks))
	}
	if chunks[0].Text != line1+"\n" {
		t.Fatalf("first chunk should end at line boundary: %q", chunks[0].Text)
	}
}

func TestSplitPreservesNewlinesAcrossManyChunks(t *testing.T) {
	// Long lines force multiple chunks; every newline in the input must
	// survive (possibly at a chunk boundary), never be dropped.
	msg := strings.Join([]string{
		strings.Repeat("一", 60),
		strings.Repeat("二", 60),
		strings.Repeat("三", 60),
	}, "\n")
	chunks, err := Split(msg, 45)
	if err != nil {
		t.Fatal(err)
	}
	joined := join(chunks)
	if strings.Count(joined, "\n") != 2 {
		t.Fatalf("expected both newlines preserved, got %d in %q", strings.Count(joined, "\n"), joined)
	}
	// Content tokens (the CJK runs) are all retained.
	if !sameTokens(strings.ReplaceAll(joined, "\n", " "), strings.ReplaceAll(msg, "\n", " ")) {
		t.Fatal("content lost while splitting across line boundaries")
	}
}

func TestSplitNeverCutsURLOrToken(t *testing.T) {
	url := "https://example.internal/long/path?token=xyz"
	// Budget comfortably exceeds the URL so it stays whole and lands in its
	// own chunk rather than being degraded.
	msg := "short head " + url + " tail words here"
	chunks, err := Split(msg, 60)
	if err != nil {
		t.Fatal(err)
	}
	joined := join(chunks)
	if !strings.Contains(joined, url) {
		t.Fatalf("atomic url must survive intact:\n got=%q\nurl=%q", joined, url)
	}
	// The URL must appear in exactly one chunk, never split across two.
	var containing int
	for _, c := range chunks {
		if strings.Contains(c.Text, "https://") {
			containing++
		}
	}
	if containing != 1 {
		t.Fatalf("url spread across %d chunks", containing)
	}
}

func TestSplitProtectedAtomOversizedDegrades(t *testing.T) {
	// A URL larger than an entire chunk cannot be split or made clickable;
	// it degrades to a summary naming the omitted character count.
	giantURL := "https://example.internal/" + strings.Repeat("segment/", 30)
	chunks, err := Split("see "+giantURL, 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range chunks {
		if strings.Contains(c.Text, "omitted") {
			found = true
		}
		if strings.Contains(c.Text, "segment/segment") {
			t.Fatalf("protected atom was cut: %q", c.Text)
		}
	}
	if !found {
		t.Fatalf("expected degradation summary, got %v", chunks)
	}
}

func TestSplitOrdinaryLongWordIsHardSplitLosslessly(t *testing.T) {
	// An ordinary unspaced word (not a URL/token) is split on rune
	// boundaries rather than dropped: content is preserved.
	giant := strings.Repeat("Z", 500)
	chunks, err := Split(giant, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 10 {
		t.Fatalf("want 10 hard-split chunks, got %d", len(chunks))
	}
	for _, c := range chunks {
		if !utf8.ValidString(c.Text) {
			t.Fatalf("invalid utf8 in hard-split chunk: %q", c.Text)
		}
		if RuneLen(render(c)) > 60 {
			t.Fatalf("chunk over budget: %d", RuneLen(render(c)))
		}
	}
	if got := join(chunks); got != giant {
		t.Fatalf("hard split must be lossless: got %d runes want 500", RuneLen(got))
	}
}

func TestSplitSequenceHeadersAndReassembly(t *testing.T) {
	msg := strings.Join([]string{
		strings.Repeat("一", 50),
		strings.Repeat("二", 50),
		strings.Repeat("三", 50),
	}, "\n")
	chunks, err := Split(msg, 40)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if c.Index != i+1 || c.Total != len(chunks) {
			t.Fatalf("bad sequence metadata on chunk %d: %+v", i, c)
		}
		rendered := render(c)
		if !strings.HasPrefix(rendered, "["+itoa(c.Index)+"/"+itoa(c.Total)+"] ") {
			t.Fatalf("missing sequence header: %q", rendered)
		}
		if RuneLen(rendered) > 40 {
			t.Fatalf("rendered chunk over budget: %d", RuneLen(rendered))
		}
	}
	if join(chunks) != msg {
		t.Fatal("ordered reassembly does not reproduce the original")
	}
}

func TestSplitSingleChunkHasNoHeader(t *testing.T) {
	chunks, err := Split("short message", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(chunks))
	}
	if got := render(chunks[0]); got != "short message" {
		t.Fatalf("single chunk must be header-free, got %q", got)
	}
}

func TestRefitShrinksToSmallerBudgetLosslessly(t *testing.T) {
	msg := strings.Repeat("数据😀 ", 40)
	base, err := Split(msg, 200)
	if err != nil {
		t.Fatal(err)
	}
	smaller, err := Refit(base, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(smaller) <= len(base) {
		t.Fatalf("smaller budget must yield at least as many fragments: base=%d refit=%d", len(base), len(smaller))
	}
	for _, c := range smaller {
		if !utf8.ValidString(c.Text) {
			t.Fatalf("refit produced invalid utf8: %q", c.Text)
		}
		if RuneLen(render(c)) > 50 {
			t.Fatalf("refit fragment over budget: %d", RuneLen(render(c)))
		}
	}
	// Sequence metadata is re-numbered contiguously 1..N.
	for i, c := range smaller {
		if c.Index != i+1 || c.Total != len(smaller) {
			t.Fatalf("non-contiguous metadata: %+v", c)
		}
	}
	// Content survives (whitespace-normalized at boundaries only).
	if !sameTokens(join(smaller), msg) {
		t.Fatal("refit lost content tokens")
	}
}

func TestRefitRejectsIllegalBudget(t *testing.T) {
	base, _ := Split(strings.Repeat("a", 100), 80)
	if _, err := Refit(base, MinBudget-1); err == nil {
		t.Fatal("Refit must reject an illegal budget")
	}
}

func TestValidateRejectsInvalidUTF8(t *testing.T) {
	bad := string([]byte{0x41, 0xff, 0xfe, 0x42})
	if err := Validate(bad); err != ErrInvalidUTF8 {
		t.Fatalf("want ErrInvalidUTF8, got %v", err)
	}
}

func TestIsBlank(t *testing.T) {
	for _, s := range []string{"", "   ", "\t\n", " \n\t\r "} {
		if !IsBlank(s) {
			t.Fatalf("%q should be blank", s)
		}
	}
	if IsBlank(" content ") {
		t.Fatal("non-blank string misclassified")
	}
}

func TestValidateBudgetRejectsIllegalAtStartup(t *testing.T) {
	for _, b := range []int{0, -5, MinBudget - 1} {
		if err := ValidateBudget(b); err == nil {
			t.Fatalf("budget %d must be rejected", b)
		}
	}
	if err := ValidateBudget(MinBudget); err != nil {
		t.Fatalf("budget %d must be accepted: %v", MinBudget, err)
	}
}

// --- helpers ----------------------------------------------------------------

func join(chunks []Chunk) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString(c.Text)
	}
	return b.String()
}

func render(c Chunk) string { return RenderChunk(c) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
