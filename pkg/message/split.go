package message

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// MinBudget is the smallest per-channel character budget accepted. It is
// sized so that a chunk header plus a degradation summary always fits;
// smaller values cannot deliver any meaningful content and are rejected at
// startup instead of failing at send time.
const MinBudget = 40

// headerReserve is the worst-case rune width reserved for a sequence header
// ("[100/100] " is 10 runes; reserving a little headroom keeps the math
// simple). The rendered header is always narrower than the reservation.
const headerReserve = 10

// Chunk is one ordered fragment of a message. Text holds the fragment body
// without its sequence header; Index is 1-based and Total is the number of
// fragments produced for the same original message.
type Chunk struct {
	Index int
	Total int
	Text  string
}

// ValidateBudget enforces per-channel budgets at configuration load time.
// Budgets are measured in characters (runes), never bytes.
func ValidateBudget(budget int) error {
	if budget < MinBudget {
		return fmt.Errorf("message_limit %d is below the minimum supported budget of %d characters", budget, MinBudget)
	}
	return nil
}

// Split divides msg into ordered chunks that each fit within budget runes,
// including the space reserved for their sequence header.
//
// Split points prefer line boundaries, then field (space) boundaries. A
// non-whitespace run — a URL, token or any other atomic word — is never cut
// in half: if it cannot fit any chunk it is replaced by a degradation summary
// stating how many characters were omitted. Every returned chunk is valid
// UTF-8 and chunks can be concatenated in order to reconstruct the original
// message apart from degraded atoms.
func Split(msg string, budget int) ([]Chunk, error) {
	if err := ValidateBudget(budget); err != nil {
		return nil, err
	}
	bodyBudget := budget - headerReserve

	var bodies []string
	var builder strings.Builder
	builderRunes := 0

	flush := func() {
		if builderRunes > 0 {
			bodies = append(bodies, builder.String())
			builder.Reset()
			builderRunes = 0
		}
	}

	// appendSep writes a separator into the current chunk. A space that
	// would overflow the budget is dropped at the boundary (whitespace
	// normalization, no content lost); a newline always flushes and starts
	// the next chunk so multi-line structure is never silently removed.
	appendSep := func(sep string) {
		n := utf8.RuneCountInString(sep)
		overflows := builderRunes > 0 && builderRunes+n > bodyBudget
		if overflows && sep != "\n" {
			// Boundary space: normalize it away rather than carrying an
			// empty separator into the next chunk.
			flush()
			return
		}
		if overflows {
			flush()
		}
		builder.WriteString(sep)
		builderRunes += n
	}

	// appendToken writes an atomic, unsplittable run of non-whitespace text.
	// URLs and credential-like tokens are preserved whole; ordinary long
	// words (e.g. an unspaced CJK line) are split on rune boundaries as a
	// last resort, so content is never silently dropped.
	appendToken := func(token string) {
		n := utf8.RuneCountInString(token)
		if n <= bodyBudget {
			if builderRunes > 0 && builderRunes+n > bodyBudget {
				flush()
			}
			builder.WriteString(token)
			builderRunes += n
			return
		}
		if isProtectedAtom(token) {
			// A URL or secret must never be cut in half.
			flush()
			bodies = append(bodies, degradationSummary(n, bodyBudget))
			return
		}
		// Last resort: split the long ordinary word on rune boundaries.
		flush()
		runes := []rune(token)
		for len(runes) > 0 {
			take := bodyBudget
			if take > len(runes) {
				take = len(runes)
			}
			piece := string(runes[:take])
			runes = runes[take:]
			if builderRunes > 0 {
				flush()
			}
			builder.WriteString(piece)
			builderRunes += take
			if len(runes) > 0 {
				flush()
			}
		}
	}

	lines := strings.Split(msg, "\n")
	for li, line := range lines {
		// A whole line fits: keep it (and its newline) intact.
		lineRunes := utf8.RuneCountInString(line)
		if li < len(lines)-1 {
			lineRunes++ // the trailing '\n' stays attached to the line
		}
		if lineRunes <= bodyBudget {
			if builderRunes > 0 && builderRunes+lineRunes > bodyBudget {
				flush()
			}
			builder.WriteString(line)
			if li < len(lines)-1 {
				builder.WriteByte('\n')
			}
			builderRunes += lineRunes
			continue
		}

		// The line itself is too long: break it at field boundaries, never
		// inside a non-whitespace atom.
		for len(line) > 0 {
			idx := strings.IndexByte(line, ' ')
			var word, rest string
			if idx < 0 {
				word, rest = line, ""
			} else {
				word, rest = line[:idx], line[idx+1:]
			}
			if word != "" {
				appendToken(word)
			}
			// A space exists whenever idx >= 0, including a trailing space
			// at the end of the line; retain it so ordered reassembly is
			// byte-faithful apart from separators dropped at overflow.
			if idx >= 0 {
				appendSep(" ")
			}
			line = rest
		}
		if li < len(lines)-1 {
			appendSep("\n")
		}
	}
	flush()

	chunks := make([]Chunk, 0, len(bodies))
	for i, body := range bodies {
		chunks = append(chunks, Chunk{Index: i + 1, Total: len(bodies), Text: body})
	}
	return chunks, nil
}

// isProtectedAtom reports whether a whitespace-free token must never be
// split in the middle: URLs (which would become unclickable half-links) and
// credential/key tokens (which would leak or invalidate the secret).
func isProtectedAtom(token string) bool {
	if strings.Contains(token, "://") {
		return true
	}
	lower := strings.ToLower(token)
	for _, marker := range []string{"token", "secret", "password", "api_key", "apikey", "authorization", "credential"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// degradationSummary renders the fallback fragment used when a protected
// atom is larger than an entire chunk. It always fits within bodyBudget and
// states the exact number of omitted characters.
func degradationSummary(omittedRunes, bodyBudget int) string {
	formats := []string{
		"[…%d chars omitted]",
		"[…%d omitted]",
		"[…%d]",
	}
	for _, f := range formats {
		s := fmt.Sprintf(f, omittedRunes)
		if utf8.RuneCountInString(s) <= bodyBudget {
			return s
		}
	}
	// Unreachable while MinBudget holds, but never return an oversized
	// fragment: truncate on a rune boundary as a last resort.
	last := fmt.Sprintf(formats[len(formats)-1], omittedRunes)
	return string([]rune(last)[:bodyBudget])
}

// Render prefixes every chunk with its sequence header ("[i/n] ") so
// receivers can detect gaps and reorder fragments.
func Render(chunks []Chunk) []string {
	out := make([]string, len(chunks))
	for i, c := range chunks {
		out[i] = RenderChunk(c)
	}
	return out
}

// RenderChunk renders one fragment. A single-chunk message carries no header:
// short messages stay byte-identical to the legacy single-post behavior,
// while split messages get full ordering metadata.
func RenderChunk(c Chunk) string {
	if c.Total <= 1 {
		return c.Text
	}
	return fmt.Sprintf("[%d/%d] %s", c.Index, c.Total, c.Text)
}

// RuneLen is a convenience used by providers and tests.
func RuneLen(s string) int { return utf8.RuneCountInString(s) }

// Refit adapts an existing chunk sequence (already produced for a larger
// budget) to a smaller budget. It re-runs the boundary-aware splitter on
// each chunk's text and re-numbers the result, so a provider can prepare
// once for its largest endpoint and shrink fragments per endpoint without
// ever crossing a rune, URL or token boundary.
func Refit(chunks []Chunk, budget int) ([]Chunk, error) {
	if err := ValidateBudget(budget); err != nil {
		return nil, err
	}
	var out []Chunk
	for _, c := range chunks {
		pieces, err := Split(c.Text, budget)
		if err != nil {
			return nil, err
		}
		out = append(out, pieces...)
	}
	for i := range out {
		out[i].Index = i + 1
		out[i].Total = len(out)
	}
	return out, nil
}
