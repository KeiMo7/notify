package rocketchat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/containrrr/shoutrrr"
	"github.com/pkg/errors"
	"go.uber.org/multierr"

	"github.com/projectdiscovery/gologger"
	msgpkg "github.com/projectdiscovery/notify/pkg/message"
	"github.com/projectdiscovery/notify/pkg/utils"
	sliceutil "github.com/projectdiscovery/utils/slice"
)

type Provider struct {
	RocketChat []*Options `yaml:"rocketchat,omitempty"`
	counter    int
}

type Options struct {
	ID                   string `yaml:"id,omitempty"`
	RocketChatWebhookURL string `yaml:"rocketchat_webhook_url,omitempty"`
	Channel              string `yaml:"rocketchat_channel,omitempty"`
	Username             string `yaml:"rocketchat_username,omitempty"`
	RocketChatFormat     string `yaml:"rocketchat_format,omitempty"`
	// MessageLimit overrides the per-channel character budget. Measured in
	// characters (runes), not bytes. Values below message.MinBudget are
	// rejected at startup.
	MessageLimit int `yaml:"message_limit,omitempty"`
}

func New(options []*Options, ids []string) (*Provider, error) {
	provider := &Provider{}

	for _, o := range options {
		if len(ids) == 0 || sliceutil.Contains(ids, o.ID) {
			provider.RocketChat = append(provider.RocketChat, o)
		}
	}

	provider.counter = 0

	if err := provider.Validate(); err != nil {
		return nil, err
	}
	return provider, nil
}

// Name identifies the channel in channel-scoped errors.
func (p *Provider) Name() string { return "rocketchat" }

// Validate rejects illegal per-channel budgets at startup.
func (p *Provider) Validate() error {
	for _, pr := range p.RocketChat {
		if pr.MessageLimit != 0 {
			if err := msgpkg.ValidateBudget(pr.MessageLimit); err != nil {
				return errors.Wrapf(err, "rocketchat id %q", pr.ID)
			}
		}
	}
	return nil
}

// budget resolves the configured limit or the channel default.
func (p *Provider) budget(pr *Options) int {
	if pr.MessageLimit > 0 {
		return pr.MessageLimit
	}
	return msgpkg.DefaultRocketChatBudget
}

// Prepare applies the RocketChat plain-text profile to an already-redacted
// message. It prepares for the largest configured endpoint budget; smaller
// endpoints shrink the fragments in SendChunks via message.Refit.
func (p *Provider) Prepare(redacted string) ([]msgpkg.Chunk, error) {
	budget := msgpkg.DefaultRocketChatBudget
	for _, pr := range p.RocketChat {
		if b := p.budget(pr); b > budget {
			budget = b
		}
	}
	return msgpkg.Process(redacted, msgpkg.Profile{Budget: budget, Syntax: msgpkg.SyntaxNone})
}

// SendChunks delivers each ordered chunk to every configured endpoint. The
// sequence header is rendered here so it survives custom format strings and
// reaches the wire exactly once.
func (p *Provider) SendChunks(chunks []msgpkg.Chunk, cliFormat string) error {
	var rocketChatErr error
	p.counter++
	for _, pr := range p.RocketChat {
		perEndpoint, err := msgpkg.Refit(chunks, p.budget(pr))
		if err != nil {
			rocketChatErr = multierr.Append(rocketChatErr, errors.Wrapf(err, "id: %s", pr.ID))
			continue
		}
		for _, chunk := range perEndpoint {
			body := utils.FormatMessage(chunk.Text, utils.SelectFormat(cliFormat, pr.RocketChatFormat), p.counter)
			msg := msgpkg.RenderChunk(msgpkg.Chunk{Index: chunk.Index, Total: chunk.Total, Text: body})
			if err := sendRocketChatChunk(pr, msg); err != nil {
				rocketChatErr = multierr.Append(rocketChatErr, errors.Wrapf(err, "id: %s chunk %d/%d", pr.ID, chunk.Index, chunk.Total))
				break
			}
		}
		gologger.Verbose().Msgf("rocketchat notification sent for id: %s", pr.ID)
	}
	return rocketChatErr
}

var rocketchatHTTPClient = &http.Client{Timeout: 15 * time.Second}

type webhookPayload struct {
	Text    string `json:"text"`
	Channel string `json:"channel,omitempty"`
	Alias   string `json:"alias,omitempty"`
}

// Send keeps the legacy single-message entry point used by package tests:
// it applies the redaction policy and endpoint budget, then sends the
// resulting chunks itself.
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

// sendRocketChatChunk posts a single already-budgeted fragment. It never
// splits again: the pipeline owns chunking so each HTTP request carries
// exactly one ordered chunk.
func sendRocketChatChunk(pr *Options, msg string) error {
	if pr.RocketChatWebhookURL == "" {
		return fmt.Errorf("rocketchat: webhook url is empty for id: %s", pr.ID)
	}

	if strings.HasPrefix(pr.RocketChatWebhookURL, "rocketchat://") {
		dest, err := rocketchatShoutrrrURL(pr)
		if err != nil {
			return errors.Wrapf(err, "failed to build rocketchat url for id: %s", pr.ID)
		}
		if err := shoutrrr.Send(dest, msg); err != nil {
			return errors.Wrapf(err, "failed to send shoutrrr rocketchat notification for id: %s", pr.ID)
		}
		return nil
	}

	payload := webhookPayload{
		Text:    msg,
		Channel: pr.Channel,
		Alias:   pr.Username,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return errors.Wrapf(err, "failed to marshal rocketchat payload for id: %s", pr.ID)
	}

	resp, err := rocketchatHTTPClient.Post(pr.RocketChatWebhookURL, "application/json", bytes.NewReader(data))
	if err != nil {
		return errors.Wrapf(err, "failed to send rocketchat notification for id: %s", pr.ID)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("rocketchat: unexpected status code %d for id: %s", resp.StatusCode, pr.ID)
	}

	return nil
}

// rocketchatShoutrrrURL copies channel and username into a rocketchat:// URL.
// Shoutrrr reads the username from the userinfo and the channel from the path segment after the two tokens.
func rocketchatShoutrrrURL(pr *Options) (string, error) {
	u, err := url.Parse(pr.RocketChatWebhookURL)
	if err != nil {
		return "", err
	}
	if pr.Username != "" && u.User.Username() == "" {
		u.User = url.User(pr.Username)
	}
	if pr.Channel != "" {
		channel := strings.TrimPrefix(pr.Channel, "#")
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			u.Path = "/" + parts[0] + "/" + parts[1] + "/" + channel
		}
	}
	return u.String(), nil
}
