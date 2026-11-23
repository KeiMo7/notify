package pushover

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
	Pushover []*Options `yaml:"pushover,omitempty"`
	counter  int
}

type Options struct {
	ID               string   `yaml:"id,omitempty"`
	PushoverApiToken string   `yaml:"pushover_api_token,omitempty"`
	UserKey          string   `yaml:"pushover_user_key,omitempty"`
	PushoverDevices  []string `yaml:"pushover_devices,omitempty"`
	PushoverFormat   string   `yaml:"pushover_format,omitempty"`
	// MessageLimit overrides the per-channel character budget (runes).
	MessageLimit int `yaml:"message_limit,omitempty"`
}

func New(options []*Options, ids []string) (*Provider, error) {
	provider := &Provider{}

	for _, o := range options {
		if len(ids) == 0 || sliceutil.Contains(ids, o.ID) {
			provider.Pushover = append(provider.Pushover, o)
		}
	}

	provider.counter = 0

	if err := provider.Validate(); err != nil {
		return nil, err
	}
	return provider, nil
}

func (p *Provider) Name() string { return "pushover" }

func (p *Provider) Validate() error {
	for _, pr := range p.Pushover {
		if pr.MessageLimit != 0 {
			if err := msgpkg.ValidateBudget(pr.MessageLimit); err != nil {
				return errors.Wrapf(err, "pushover id %q", pr.ID)
			}
		}
	}
	return nil
}

func (p *Provider) budget() int {
	budget := msgpkg.DefaultPushoverBudget
	for _, pr := range p.Pushover {
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
	return msgpkg.DefaultPushoverBudget
}

func (p *Provider) Prepare(redacted string) ([]msgpkg.Chunk, error) {
	return msgpkg.Process(redacted, msgpkg.Profile{Budget: p.budget(), Syntax: msgpkg.SyntaxNone})
}

func (p *Provider) SendChunks(chunks []msgpkg.Chunk, cliFormat string) error {
	var pushoverErr error
	p.counter++
	for _, pr := range p.Pushover {
		perEndpoint, err := msgpkg.Refit(chunks, p.endpointBudget(pr))
		if err != nil {
			pushoverErr = multierr.Append(pushoverErr, errors.Wrapf(err, "id: %s", pr.ID))
			continue
		}
		for _, chunk := range perEndpoint {
			body := utils.FormatMessage(chunk.Text, utils.SelectFormat(cliFormat, pr.PushoverFormat), p.counter)
			msg := msgpkg.RenderChunk(msgpkg.Chunk{Index: chunk.Index, Total: chunk.Total, Text: body})

			url := fmt.Sprintf("pushover://shoutrrr:%s@%s/?devices=%s", pr.PushoverApiToken, pr.UserKey, strings.Join(pr.PushoverDevices, ","))
			if err := shoutrrr.Send(url, msg); err != nil {
				pushoverErr = multierr.Append(pushoverErr, errors.Wrapf(err,
					"failed to send pushover notification for id: %s chunk %d/%d", pr.ID, chunk.Index, chunk.Total))
				break
			}
		}
		gologger.Verbose().Msgf("pushover notification sent for id: %s", pr.ID)
	}
	return pushoverErr
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
