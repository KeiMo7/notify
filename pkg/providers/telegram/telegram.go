package telegram

import (
	"fmt"
	"strings"

	"github.com/containrrr/shoutrrr"
	"github.com/pkg/errors"
	"go.uber.org/multierr"

	"github.com/projectdiscovery/gologger"
	msgpkg "github.com/projectdiscovery/notify/pkg/message"
	"github.com/projectdiscovery/notify/pkg/utils"
	sliceutil "github.com/projectdiscovery/utils/slice"
)

type Provider struct {
	Telegram []*Options `yaml:"telegram,omitempty"`
	counter  int
}

type Options struct {
	ID                string `yaml:"id,omitempty"`
	TelegramAPIKey    string `yaml:"telegram_api_key,omitempty"`
	TelegramChatID    string `yaml:"telegram_chat_id,omitempty"`
	TelegramThreadID  string `yaml:"telegram_thread_id,omitempty"`
	TelegramFormat    string `yaml:"telegram_format,omitempty"`
	TelegramParseMode string `yaml:"telegram_parsemode,omitempty"`
	// TelegramPreview enables link previews. Previews stay off unless this is set.
	TelegramPreview bool `yaml:"telegram_preview,omitempty"`
	// MessageLimit overrides the per-channel character budget (runes).
	MessageLimit int `yaml:"message_limit,omitempty"`
}

func New(options []*Options, ids []string) (*Provider, error) {
	provider := &Provider{}

	for _, o := range options {
		if len(ids) == 0 || sliceutil.Contains(ids, o.ID) {
			provider.Telegram = append(provider.Telegram, o)
		}
	}

	provider.counter = 0

	if err := provider.Validate(); err != nil {
		return nil, err
	}
	return provider, nil
}

func (p *Provider) Name() string { return "telegram" }

func (p *Provider) Validate() error {
	for _, pr := range p.Telegram {
		if pr.MessageLimit != 0 {
			if err := msgpkg.ValidateBudget(pr.MessageLimit); err != nil {
				return errors.Wrapf(err, "telegram id %q", pr.ID)
			}
		}
	}
	return nil
}

func (p *Provider) budget() int {
	budget := msgpkg.DefaultTelegramBudget
	for _, pr := range p.Telegram {
		if pr.MessageLimit > 0 && pr.MessageLimit > budget {
			budget = pr.MessageLimit
		}
	}
	return budget
}

// profile resolves the syntax of the strictest configured endpoint.
func (p *Provider) profile() msgpkg.Profile {
	return msgpkg.Profile{Budget: p.budget(), Syntax: p.syntax()}
}

func (p *Provider) syntax() msgpkg.Syntax {
	// Different endpoints may use different parse modes; escaping for the
	// most restrictive mode keeps every endpoint safe.
	rank := map[msgpkg.Syntax]int{
		msgpkg.SyntaxNone:               0,
		msgpkg.SyntaxTelegramMarkdown:   1,
		msgpkg.SyntaxTelegramHTML:       2,
		msgpkg.SyntaxTelegramMarkdownV2: 3,
	}
	chosen := msgpkg.SyntaxNone
	for _, pr := range p.Telegram {
		s := msgpkg.ParseTelegramSyntax(pr.TelegramParseMode)
		if rank[s] > rank[chosen] {
			chosen = s
		}
	}
	return chosen
}

func (p *Provider) endpointBudget(pr *Options) int {
	if pr.MessageLimit > 0 {
		return pr.MessageLimit
	}
	return msgpkg.DefaultTelegramBudget
}

func (p *Provider) Prepare(redacted string) ([]msgpkg.Chunk, error) {
	return msgpkg.Process(redacted, p.profile())
}

func (p *Provider) SendChunks(chunks []msgpkg.Chunk, cliFormat string) error {
	var telegramErr error
	p.counter++
	for _, pr := range p.Telegram {
		parseMode := pr.TelegramParseMode
		if parseMode == "" {
			parseMode = "None"
		}
		perEndpoint, err := msgpkg.Refit(chunks, p.endpointBudget(pr))
		if err != nil {
			telegramErr = multierr.Append(telegramErr, errors.Wrapf(err, "id: %s", pr.ID))
			continue
		}
		for _, chunk := range perEndpoint {
			body := utils.FormatMessage(chunk.Text, utils.SelectFormat(cliFormat, pr.TelegramFormat), p.counter)
			msg := msgpkg.RenderChunk(msgpkg.Chunk{Index: chunk.Index, Total: chunk.Total, Text: body})
			url := telegramURL(pr, parseMode)
			if err := shoutrrr.Send(url, msg); err != nil {
				telegramErr = multierr.Append(telegramErr, errors.Wrapf(err,
					"failed to send telegram notification for id: %s chunk %d/%d", pr.ID, chunk.Index, chunk.Total))
				break
			}
		}
		gologger.Verbose().Msgf("telegram notification sent for id: %s", pr.ID)
	}
	return telegramErr
}

// Send is the legacy single-message entry point retained for package tests.
func (p *Provider) Send(message, cliFormat string) error {
	chunks, err := p.Prepare(msgpkg.Redact(message))
	if err != nil {
		return err
	}
	if len(chunks) == 0 {
		return nil
	}
	return p.SendChunks(chunks, cliFormat)
}

func telegramURL(pr *Options, parseMode string) string {
	preview := "No"
	if pr.TelegramPreview {
		preview = "Yes"
	}
	channel := pr.TelegramChatID
	if pr.TelegramThreadID != "" && !strings.Contains(pr.TelegramChatID, ":") {
		channel = fmt.Sprintf("%s:%s", pr.TelegramChatID, pr.TelegramThreadID)
	}
	return fmt.Sprintf("telegram://%s@telegram?channels=%s&parsemode=%s&preview=%s", pr.TelegramAPIKey, channel, parseMode, preview)
}
