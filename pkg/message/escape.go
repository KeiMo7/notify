package message

import "strings"

// Escapers in this file only protect channel-specific markup syntax. They
// never rewrite semantics: escaping '<' to '&lt;' changes representation,
// not meaning, and the receiving client renders the original character.
// Transport-level encodings (JSON string escaping, HTML field encoding for
// Teams) stay inside the respective providers' payload builders.

// EscapeTelegramHTML escapes the characters special to Telegram's HTML
// parse mode. Use only when telegram_parsemode is HTML.
func EscapeTelegramHTML(msg string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	)
	return replacer.Replace(msg)
}

// EscapeTelegramMarkdown escapes the characters special to Telegram's
// legacy Markdown parse mode. Use only when telegram_parsemode is Markdown.
// MarkdownV2 needs a wider character set and is handled separately.
func EscapeTelegramMarkdown(msg string) string {
	replacer := strings.NewReplacer(
		"_", "\\_",
		"*", "\\*",
		"`", "\\`",
		"[", "\\[",
	)
	return replacer.Replace(msg)
}

// EscapeTelegramMarkdownV2 escapes the full reserved character set defined
// by Telegram's MarkdownV2 parse mode.
func EscapeTelegramMarkdownV2(msg string) string {
	specials := []string{
		"_", "*", "[", "]", "(", ")", "~", "`", ">",
		"#", "+", "-", "=", "|", "{", "}", ".", "!",
	}
	var b strings.Builder
	b.Grow(len(msg))
	runes := []rune(msg)
	for _, r := range runes {
		if strings.ContainsRune(strings.Join(specials, ""), r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// EscapeChatMarkdown escapes the mrkdwn metacharacters shared by Slack and
// Discord plain-text posts so alert content cannot be reinterpreted as
// formatting (e.g. an underscore inside a hostname starting italics).
func EscapeChatMarkdown(msg string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	)
	return replacer.Replace(msg)
}
