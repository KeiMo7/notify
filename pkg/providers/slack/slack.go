package slack

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/pkg/errors"
	"go.uber.org/multierr"

	"github.com/projectdiscovery/gologger"
	msgpkg "github.com/projectdiscovery/notify/pkg/message"
	"github.com/projectdiscovery/notify/pkg/utils"
	sliceutil "github.com/projectdiscovery/utils/slice"
)

type Provider struct {
	Slack   []*Options `yaml:"slack,omitempty"`
	counter int
}

type Options struct {
	ID              string `yaml:"id,omitempty"`
	SlackWebHookURL string `yaml:"slack_webhook_url,omitempty"`
	SlackUsername   string `yaml:"slack_username,omitempty"`
	SlackChannel    string `yaml:"slack_channel,omitempty"`
	SlackThreads    bool   `yaml:"slack_threads,omitempty"`
	SlackThreadTS   string `yaml:"slack_thread_ts,omitempty"`
	SlackToken      string `yaml:"slack_token,omitempty"`
	SlackFormat     string `yaml:"slack_format,omitempty"`
	SlackIconEmoji  string `yaml:"slack_icon_emoji,omitempty"`
	// MessageLimit overrides the per-channel character budget (runes).
	MessageLimit int `yaml:"message_limit,omitempty"`
}

func New(options []*Options, ids []string) (*Provider, error) {
	provider := &Provider{}

	for _, o := range options {
		if len(ids) == 0 || sliceutil.Contains(ids, o.ID) {
			provider.Slack = append(provider.Slack, o)
		}
	}

	provider.counter = 0

	if err := provider.Validate(); err != nil {
		return nil, err
	}
	return provider, nil
}

func (p *Provider) Name() string { return "slack" }

func (p *Provider) Validate() error {
	for _, pr := range p.Slack {
		if pr.MessageLimit != 0 {
			if err := msgpkg.ValidateBudget(pr.MessageLimit); err != nil {
				return errors.Wrapf(err, "slack id %q", pr.ID)
			}
		}
	}
	return nil
}

func (p *Provider) budget() int {
	budget := msgpkg.DefaultSlackBudget
	for _, pr := range p.Slack {
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
	return msgpkg.DefaultSlackBudget
}

func (p *Provider) Prepare(redacted string) ([]msgpkg.Chunk, error) {
	return msgpkg.Process(redacted, msgpkg.Profile{Budget: p.budget(), Syntax: msgpkg.SyntaxChatMarkdown})
}

func (p *Provider) SendChunks(chunks []msgpkg.Chunk, cliFormat string) error {
	var slackErr error
	p.counter++
	for _, pr := range p.Slack {
		perEndpoint, err := msgpkg.Refit(chunks, p.endpointBudget(pr))
		if err != nil {
			slackErr = multierr.Append(slackErr, errors.Wrapf(err, "id: %s", pr.ID))
			continue
		}
		for _, chunk := range perEndpoint {
			body := utils.FormatMessage(chunk.Text, utils.SelectFormat(cliFormat, pr.SlackFormat), p.counter)
			msg := msgpkg.RenderChunk(msgpkg.Chunk{Index: chunk.Index, Total: chunk.Total, Text: body})
			if err := pr.sendOne(msg); err != nil {
				slackErr = multierr.Append(slackErr, errors.Wrapf(err, "id: %s chunk %d/%d", pr.ID, chunk.Index, chunk.Total))
				break
			}
		}
		gologger.Verbose().Msgf("Slack notification sent for id: %s", pr.ID)
	}
	return slackErr
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

// sendOne delivers a single already-budgeted fragment through either the
// threaded API path or the incoming-webhook path.
func (options *Options) sendOne(msg string) error {
	// Handle threaded messages separately
	if options.SlackThreads {
		if options.SlackToken == "" {
			return errors.Wrap(fmt.Errorf("slack_token value is required to start a thread"),
				fmt.Sprintf("failed to send slack notification for id: %s ", options.ID))
		}
		if options.SlackChannel == "" {
			return errors.Wrap(fmt.Errorf("slack_channel value is required to start a thread"),
				fmt.Sprintf("failed to send slack notification for id: %s ", options.ID))
		}
		return options.SendThreaded(msg)
	}

	// Send via webhook with emoji and username
	if !strings.HasPrefix(options.SlackWebHookURL, "https://hooks.slack.com/services/") {
		return errors.Wrap(fmt.Errorf("invalid slack webhook URL"),
			fmt.Sprintf("failed to send slack notification for id: %s ", options.ID))
	}

	payload := map[string]interface{}{
		"text": msg,
	}
	if options.SlackUsername != "" {
		payload["username"] = options.SlackUsername
	}
	if options.SlackIconEmoji != "" {
		payload["icon_emoji"] = options.SlackIconEmoji
	}

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("failed to marshal Slack payload for id: %s", options.ID))
	}

	resp, err := http.Post(options.SlackWebHookURL, "application/json", bytes.NewBuffer(jsonPayload))
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("failed to send slack notification for id: %s", options.ID))
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 400 {
		return errors.Wrap(fmt.Errorf("received non-success status: %s", resp.Status), fmt.Sprintf("failed to send slack notification for id: %s", options.ID))
	}
	return nil
}
