package runner

import (
	"bufio"
	"fmt"
	"unicode/utf8"
)

var ellipsis = []byte("...")

// runeLimit is the per-token input budget in characters (runes). All
// truncation in these splitters is rune-aware so a Chinese character or an
// emoji can never be cut into an invalid byte sequence.
//
// Note: this is the input-stage segmentation budget. Each delivery channel
// applies its own, smaller character budget in pkg/message, so even tokens
// that fit here can be split again per channel.
func bulkSplitter(charLimit int) (bufio.SplitFunc, error) {
	if charLimit <= utf8.RuneCount(ellipsis) {
		return nil, fmt.Errorf("charLimit must be > %d", utf8.RuneCount(ellipsis))
	}
	return func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		var lineAdvance int
		var line []byte
		tokenRunes := 0

		// Keep getting lines until we exceed charLimit (counted in runes).
		for {
			lineAdvance, line, err = bufio.ScanLines(data[advance:], atEOF)

			if err != nil || line == nil {
				// Failed to get a line
				if atEOF {
					// We're done
					break
				}

				if utf8.RuneCount(data) < charLimit {
					// We need more data
					return 0, nil, nil
				}
				// We have enough data, but bufio.ScanLines couldn't see a
				// newline in what's left. Handle it so the scanner never
				// returns bufio.ErrTooLong.
				if tokenRunes == 0 {
					// Even just the first line is too much: truncate on a
					// rune boundary and give it.
					return truncateRunes(data, charLimit)
				}
				// Give what we had
				break
			}

			lineRunes := utf8.RuneCount(line) + 1 // +1 for the joined '\n'
			if tokenRunes+lineRunes > charLimit {
				if tokenRunes == 0 {
					// Even just the first line is too much: truncate on a
					// rune boundary and give it.
					return truncateRunes(line, charLimit)
				}
				// Give what we had
				break
			}

			advance += lineAdvance
			token = append(token, line...)
			token = append(token, '\n')
			tokenRunes += lineRunes
		}

		if len(token) > 0 {
			// It'll have a trailing newline; remove it (single ASCII byte).
			token = token[:len(token)-1]
		}
		return
	}, nil
}

// lineLengthSplitter returns a bufio.SplitFunc that splits on all newlines
// while never emitting more than charLimit runes per token.
func lineLengthSplitter(charLimit int) (bufio.SplitFunc, error) {
	if charLimit <= utf8.RuneCount(ellipsis) {
		return nil, fmt.Errorf("charLimit must be > %d", utf8.RuneCount(ellipsis))
	}
	return func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		var line []byte

		// Get a line
		advance, line, err = bufio.ScanLines(data[advance:], atEOF)

		if !atEOF && (err != nil || line == nil) {
			if utf8.RuneCount(data) < charLimit {
				// We need more data
				return 0, nil, nil
			}
			// Enough data but no newline in sight; truncate safely.
			return truncateRunes(data, charLimit)
		}

		if utf8.RuneCount(line) > charLimit {
			// Got too much: truncate on a rune boundary.
			return truncateRunes(line, charLimit)
		}

		return advance, line, err
	}, nil
}

// truncateRunes returns the first (charLimit - len("...")) runes of data
// followed by an ellipsis, plus the byte count consumed. It never slices in
// the middle of a multi-byte rune, so the returned token is always valid
// UTF-8.
func truncateRunes(data []byte, charLimit int) (int, []byte, error) {
	keep := charLimit - utf8.RuneCount(ellipsis)
	runes := 0
	cut := 0
	for cut < len(data) && runes < keep {
		_, size := utf8.DecodeRune(data[cut:])
		cut += size
		runes++
	}
	token := make([]byte, 0, cut+len(ellipsis))
	token = append(token, data[:cut]...)
	token = append(token, ellipsis...)
	return cut, token, nil
}
