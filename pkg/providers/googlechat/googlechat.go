package googlechat

import (
	"fmt"

	"github.com/containrrr/shoutrrr"
	"github.com/pkg/errors"
	"go.uber.org/multierr"

	"github.com/projectdiscovery/gologger"
	msgpkg "github.com/projectdiscovery/notify/pkg/message"
	"github.com/projectdiscovery/notify/pkg/utils"
	sliceutil "github.com/projectdiscovery/utils/slice"
)

type Provider struct {
	GoogleChat []*Options `yaml:"googleChat,omitempty"`
	counter    int
}

type Options struct {
	ID               string `yaml:"id,omitempty"`
	Space            string `yaml:"space,omitempty"`
	Key              string `yaml:"key,omitempty"`
	Token            string `yaml:"token,omitempty"`
	GoogleChatFormat string `yaml:"google_chat_format,omitempty"`
	// MessageLimit overrides the per-channel character budget (runes).
	MessageLimit int `yaml:"message_limit,omitempty"`
}

func New(options []*Options, ids []string) (*Provider, error) {
	provider := &Provider{}

	for _, o := range options {
		if len(ids) == 0 || sliceutil.Contains(ids, o.ID) {
			provider.GoogleChat = append(provider.GoogleChat, o)
		}
	}

	provider.counter = 0

	if err := provider.Validate(); err != nil {
		return nil, err
	}
	return provider, nil
}

func (p *Provider) Name() string { return "googlechat" }

func (p *Provider) Validate() error {
	for _, pr := range p.GoogleChat {
		if pr.MessageLimit != 0 {
			if err := msgpkg.ValidateBudget(pr.MessageLimit); err != nil {
				return errors.Wrapf(err, "googlechat id %q", pr.ID)
			}
		}
	}
	return nil
}

func (p *Provider) budget() int {
	budget := msgpkg.DefaultGoogleChatBudget
	for _, pr := range p.GoogleChat {
		if pr.MessageLimit > 0 && pr.MessageLimit > budget {
			budget = pr.MessageLimit
		}
	}
	return budget
}

func (p *Provider) endpointBudget(pr *Options) int {
	if pr.MessageLimit > 0 {
		return pr.MessageLimit
	}
	return msgpkg.DefaultGoogleChatBudget
}

func (p *Provider) Prepare(redacted string) ([]msgpkg.Chunk, error) {
	return msgpkg.Process(redacted, msgpkg.Profile{Budget: p.budget(), Syntax: msgpkg.SyntaxNone})
}

func (p *Provider) SendChunks(chunks []msgpkg.Chunk, cliFormat string) error {
	var googleChatErr error
	p.counter++
	for _, pr := range p.GoogleChat {
		perEndpoint, err := msgpkg.Refit(chunks, p.endpointBudget(pr))
		if err != nil {
			googleChatErr = multierr.Append(googleChatErr, errors.Wrapf(err, "id: %s", pr.ID))
			continue
		}
		for _, chunk := range perEndpoint {
			body := utils.FormatMessage(chunk.Text, utils.SelectFormat(cliFormat, pr.GoogleChatFormat), p.counter)
			msg := msgpkg.RenderChunk(msgpkg.Chunk{Index: chunk.Index, Total: chunk.Total, Text: body})
			url := fmt.Sprintf("googlechat://chat.googleapis.com/v1/spaces/%s/messages?key=%s&token=%s", pr.Space, pr.Key, pr.Token)
			if err := shoutrrr.Send(url, msg); err != nil {
				googleChatErr = multierr.Append(googleChatErr, errors.Wrapf(err,
					"failed to send googleChat notification for id: %s chunk %d/%d", pr.ID, chunk.Index, chunk.Total))
				break
			}
		}
		gologger.Verbose().Msgf("googleChat notification sent for id: %s", pr.ID)
	}
	return googleChatErr
}

// Send is the legacy single-message entry point.
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
