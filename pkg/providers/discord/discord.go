package discord

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/oriser/regroup"
	"github.com/pkg/errors"
	"go.uber.org/multierr"

	"github.com/projectdiscovery/gologger"
	msgpkg "github.com/projectdiscovery/notify/pkg/message"
	"github.com/projectdiscovery/notify/pkg/utils"
	"github.com/projectdiscovery/notify/pkg/utils/httpreq"
	sliceutil "github.com/projectdiscovery/utils/slice"
)

var reDiscordWebhook = regroup.MustCompile(`(?P<scheme>https?):\/\/(?P<domain>(?:ptb\.|canary\.)?discord(?:app)?\.com)\/api(?:\/)?(?P<api_version>v\d{1,2})?\/webhooks\/(?P<webhook_identifier>\d{17,19})\/(?P<webhook_token>[\w\-]{68})`)

type Provider struct {
	Discord []*Options `yaml:"discord,omitempty"`
	counter int
}

type Options struct {
	ID                      string `yaml:"id,omitempty"`
	DiscordWebHookURL       string `yaml:"discord_webhook_url,omitempty"`
	DiscordWebHookUsername  string `yaml:"discord_username,omitempty"`
	DiscordWebHookAvatarURL string `yaml:"discord_avatar,omitempty"`
	DiscordThreads          bool   `yaml:"discord_threads,omitempty"`
	DiscordThreadID         string `yaml:"discord_thread_id,omitempty"`
	DiscordFormat           string `yaml:"discord_format,omitempty"`
	// MessageLimit overrides the per-channel character budget (runes).
	MessageLimit int `yaml:"message_limit,omitempty"`
}

func New(options []*Options, ids []string) (*Provider, error) {
	provider := &Provider{}

	for _, o := range options {
		if len(ids) == 0 || sliceutil.Contains(ids, o.ID) {
			provider.Discord = append(provider.Discord, o)
		}
	}

	provider.counter = 0

	if err := provider.Validate(); err != nil {
		return nil, err
	}
	return provider, nil
}

func (p *Provider) Name() string { return "discord" }

func (p *Provider) Validate() error {
	for _, pr := range p.Discord {
		if pr.MessageLimit != 0 {
			if err := msgpkg.ValidateBudget(pr.MessageLimit); err != nil {
				return errors.Wrapf(err, "discord id %q", pr.ID)
			}
		}
	}
	return nil
}

func (p *Provider) budget() int {
	budget := msgpkg.DefaultDiscordBudget
	for _, pr := range p.Discord {
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
	return msgpkg.DefaultDiscordBudget
}

func (p *Provider) Prepare(redacted string) ([]msgpkg.Chunk, error) {
	return msgpkg.Process(redacted, msgpkg.Profile{Budget: p.budget(), Syntax: msgpkg.SyntaxChatMarkdown})
}

func (p *Provider) SendChunks(chunks []msgpkg.Chunk, cliFormat string) error {
	var errs error
	p.counter++
	for _, pr := range p.Discord {
		perEndpoint, err := msgpkg.Refit(chunks, p.endpointBudget(pr))
		if err != nil {
			errs = multierr.Append(errs, errors.Wrapf(err, "id: %s", pr.ID))
			continue
		}
		for _, chunk := range perEndpoint {
			body := utils.FormatMessage(chunk.Text, utils.SelectFormat(cliFormat, pr.DiscordFormat), p.counter)
			msg := msgpkg.RenderChunk(msgpkg.Chunk{Index: chunk.Index, Total: chunk.Total, Text: body})
			if err := pr.sendOne(msg); err != nil {
				errs = multierr.Append(errs, errors.Wrapf(err, "id: %s chunk %d/%d", pr.ID, chunk.Index, chunk.Total))
				break
			}
		}
		gologger.Verbose().Msgf("discord notification sent for id: %s", pr.ID)
	}
	return errs
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

// sendOne posts one already-budgeted fragment. JSON encoding is handled by
// json.Marshal, so content characters need no manual escaping beyond the
// mrkdwn protection applied in Prepare.
func (options *Options) sendOne(message string) error {
	if options.DiscordThreads {
		if options.DiscordThreadID == "" {
			return fmt.Errorf("thread_id value is required when discord_threads is set to true. check your configuration at id: %s", options.ID)
		}
		return options.SendThreaded(message)
	}

	matchedGroups, err := reDiscordWebhook.Groups(options.DiscordWebHookURL)
	if err != nil {
		return fmt.Errorf("incorrect discord configuration for id: %s ", options.ID)
	}

	webhookID, webhookToken := matchedGroups["webhook_identifier"], matchedGroups["webhook_token"]
	scheme, domain := matchedGroups["scheme"], matchedGroups["domain"]
	apiVersion := matchedGroups["api_version"]
	base := fmt.Sprintf("%s://%s/api", scheme, domain)
	if apiVersion != "" {
		base += "/" + apiVersion
	}
	webhookURL := fmt.Sprintf("%s/webhooks/%s/%s?splitlines=no", base, webhookID, webhookToken)

	payload := APIRequest{
		Content:   message,
		Username:  options.DiscordWebHookUsername,
		AvatarURL: options.DiscordWebHookAvatarURL,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return errors.Wrapf(err, "failed to marshal discord payload for id: %s", options.ID)
	}

	req, err := http.NewRequest(http.MethodPost, webhookURL, bytes.NewReader(encoded))
	if err != nil {
		return errors.Wrapf(err, "failed to build discord request for id: %s", options.ID)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpreq.NewClient().Do(req)
	if err != nil {
		return errors.Wrapf(err, "failed to send discord notification for id: %s ", options.ID)
	}
	if resp != nil {
		_ = resp.Body.Close()
		if resp.StatusCode >= 400 {
			return fmt.Errorf("discord: received non-success status %s for id: %s", resp.Status, options.ID)
		}
	}
	return nil
}
