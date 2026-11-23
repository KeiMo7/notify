package message

import (
	"strings"
)

// Syntax identifies the target channel's markup grammar so the pipeline can
// pick the right escaper. SyntaxNone means the channel takes plain text.
type Syntax int

const (
	SyntaxNone Syntax = iota
	SyntaxTelegramHTML
	SyntaxTelegramMarkdown
	SyntaxTelegramMarkdownV2
	SyntaxChatMarkdown
)

// Profile captures the per-channel contract: its character budget and the
// markup grammar the receiver parses. Every channel gets its own Profile;
// the global -char-limit flag never overrides these budgets.
type Profile struct {
	// Budget is the maximum number of characters (runes) per rendered
	// chunk, including the sequence header.
	Budget int
	Syntax Syntax
}

// Default per-channel budgets, chosen from each platform's documented
// message limits minus headroom for envelope fields.
const (
	DefaultSlackBudget      = 3500  // Slack text limit is 40000; conservative default
	DefaultDiscordBudget    = 1900  // Discord content limit 2000
	DefaultTelegramBudget   = 4000  // Telegram text limit 4096
	DefaultPushoverBudget   = 900   // Pushover message limit 1024
	DefaultSMTPBudget       = 9000  // SMTP has no hard cap; keep bodies manageable
	DefaultTeamsBudget      = 20000 // Teams payload limit ~28KB
	DefaultGoogleChatBudget = 4000  // Google Chat message limit 4096
	DefaultGotifyBudget     = 3500  // Gotify practical limit
	DefaultRocketChatBudget = 4000  // RocketChat message limit 4000
	DefaultCustomBudget     = 4000  // Unknown endpoint: conservative default
)

// Process runs the full channel-specific pipeline against a raw message:
//
//  1. Validate encoding (invalid UTF-8 fails loudly, original is retained).
//  2. Drop blank messages (returns nil chunks, no error).
//  3. Apply the shared redaction policy.
//  4. Escape only the target channel's markup syntax.
//  5. Split into budgeted, header-bearing ordered chunks.
//
// The same redaction runs for every channel and every log line, so secrets
// are masked once and uniformly.
func Process(raw string, profile Profile) ([]Chunk, error) {
	if err := Validate(raw); err != nil {
		return nil, err
	}
	if IsBlank(raw) {
		return nil, nil
	}

	redacted := Redact(raw)
	escaped := ApplySyntax(redacted, profile.Syntax)
	return Split(escaped, profile.Budget)
}

// ApplySyntax dispatches to the escaper matching the channel grammar.
func ApplySyntax(msg string, syntax Syntax) string {
	switch syntax {
	case SyntaxTelegramHTML:
		return EscapeTelegramHTML(msg)
	case SyntaxTelegramMarkdown:
		return EscapeTelegramMarkdown(msg)
	case SyntaxTelegramMarkdownV2:
		return EscapeTelegramMarkdownV2(msg)
	case SyntaxChatMarkdown:
		return EscapeChatMarkdown(msg)
	default:
		return msg
	}
}

// ParseTelegramSyntax maps a telegram_parsemode config value to a Syntax.
// Empty/None/Plain mean plain text and require no escaping.
func ParseTelegramSyntax(parseMode string) Syntax {
	switch strings.ToLower(strings.TrimSpace(parseMode)) {
	case "html":
		return SyntaxTelegramHTML
	case "markdown":
		return SyntaxTelegramMarkdown
	case "markdownv2":
		return SyntaxTelegramMarkdownV2
	default:
		return SyntaxNone
	}
}

// ResolveBudget returns the configured budget when set, otherwise the
// channel default. It validates the result so illegal values fail at
// startup.
func ResolveBudget(configured, defaultBudget int) (int, error) {
	budget := defaultBudget
	if configured > 0 {
		budget = configured
	}
	if err := ValidateBudget(budget); err != nil {
		return 0, err
	}
	return budget, nil
}
