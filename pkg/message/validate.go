package message

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// ErrInvalidUTF8 is returned when an incoming message is not valid UTF-8.
// Callers must surface this error instead of silently replacing bytes with
// '?' — the original content is preserved on the error path untouched.
var ErrInvalidUTF8 = errors.New("message is not valid UTF-8; refusing to send rather than replacing bytes")

// Validate checks the wire encoding of a raw message. It returns the message
// unchanged on success or ErrInvalidUTF8 so callers can retain the original
// bytes for diagnostics.
func Validate(msg string) error {
	if !utf8.ValidString(msg) {
		return ErrInvalidUTF8
	}
	return nil
}

// IsBlank reports whether a message carries no content after trimming
// whitespace. Blank messages are dropped rather than sent as empty
// notifications.
func IsBlank(msg string) bool {
	return strings.TrimSpace(msg) == ""
}
