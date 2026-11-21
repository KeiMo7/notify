package providers

import (
	"fmt"
	"strings"

	"github.com/acarl005/stripansi"
	"github.com/pkg/errors"
	"go.uber.org/multierr"

	"github.com/projectdiscovery/gologger"
	"github.com/projectdiscovery/notify/pkg/providers/custom"
	"github.com/projectdiscovery/notify/pkg/providers/discord"
	"github.com/projectdiscovery/notify/pkg/providers/googlechat"
	"github.com/projectdiscovery/notify/pkg/providers/gotify"
	"github.com/projectdiscovery/notify/pkg/providers/pushover"
	"github.com/projectdiscovery/notify/pkg/providers/rocketchat"
	"github.com/projectdiscovery/notify/pkg/providers/slack"
	"github.com/projectdiscovery/notify/pkg/providers/smtp"
	"github.com/projectdiscovery/notify/pkg/providers/teams"
	"github.com/projectdiscovery/notify/pkg/providers/telegram"
	"github.com/projectdiscovery/notify/pkg/types"
	sliceutil "github.com/projectdiscovery/utils/slice"
)

// ProviderOptions is configuration for notify providers
type ProviderOptions struct {
	Slack      []*slack.Options      `yaml:"slack,omitempty"`
	Discord    []*discord.Options    `yaml:"discord,omitempty"`
	Pushover   []*pushover.Options   `yaml:"pushover,omitempty"`
	SMTP       []*smtp.Options       `yaml:"smtp,omitempty"`
	Teams      []*teams.Options      `yaml:"teams,omitempty"`
	Telegram   []*telegram.Options   `yaml:"telegram,omitempty"`
	GoogleChat []*googlechat.Options `yaml:"googlechat,omitempty"`
	Custom     []*custom.Options     `yaml:"custom,omitempty"`
	Gotify     []*gotify.Options     `yaml:"gotify,omitempty"`
	RocketChat []*rocketchat.Options `yaml:"rocketchat,omitempty"`
}

// Provider is an interface implemented by providers
type Provider interface {
	Send(message, CliFormat string) error
}

type Client struct {
	providers       []Provider
	providerOptions *ProviderOptions
	options         *types.Options
}

func New(providerOptions *ProviderOptions, options *types.Options) (*Client, error) {

	client := &Client{providerOptions: providerOptions, options: options}

	if providerOptions.Slack != nil && (len(options.Providers) == 0 || sliceutil.Contains(options.Providers, "slack")) {

		provider, err := slack.New(providerOptions.Slack, options.IDs)
		if err != nil {
			return nil, errors.Wrap(err, "could not create slack provider client")
		}

		client.providers = append(client.providers, provider)
	}
	if providerOptions.Discord != nil && (len(options.Providers) == 0 || sliceutil.Contains(options.Providers, "discord")) {

		provider, err := discord.New(providerOptions.Discord, options.IDs)
		if err != nil {
			return nil, errors.Wrap(err, "could not create discord provider client")
		}
		client.providers = append(client.providers, provider)
	}
	if providerOptions.Pushover != nil && (len(options.Providers) == 0 || sliceutil.Contains(options.Providers, "pushover")) {

		provider, err := pushover.New(providerOptions.Pushover, options.IDs)
		if err != nil {
			return nil, errors.Wrap(err, "could not create pushover provider client")
		}
		client.providers = append(client.providers, provider)
	}
	if providerOptions.GoogleChat != nil && (len(options.Providers) == 0 || sliceutil.Contains(options.Providers, "googlechat")) {

		provider, err := googlechat.New(providerOptions.GoogleChat, options.IDs)
		if err != nil {
			return nil, errors.Wrap(err, "could not create googlechat provider client")
		}
		client.providers = append(client.providers, provider)
	}
	if providerOptions.SMTP != nil && (len(options.Providers) == 0 || sliceutil.Contains(options.Providers, "smtp")) {

		provider, err := smtp.New(providerOptions.SMTP, options.IDs)
		if err != nil {
			return nil, errors.Wrap(err, "could not create smtp provider client")
		}
		client.providers = append(client.providers, provider)
	}
	if providerOptions.Teams != nil && (len(options.Providers) == 0 || sliceutil.Contains(options.Providers, "teams")) {

		provider, err := teams.New(providerOptions.Teams, options.IDs)
		if err != nil {
			return nil, errors.Wrap(err, "could not create teams provider client")
		}
		client.providers = append(client.providers, provider)
	}
	if providerOptions.Telegram != nil && (len(options.Providers) == 0 || sliceutil.Contains(options.Providers, "telegram")) {

		provider, err := telegram.New(providerOptions.Telegram, options.IDs)
		if err != nil {
			return nil, errors.Wrap(err, "could not create telegram provider client")
		}
		client.providers = append(client.providers, provider)
	}

	if providerOptions.Custom != nil && (len(options.Providers) == 0 || sliceutil.Contains(options.Providers, "custom")) {

		provider, err := custom.New(providerOptions.Custom, options.IDs)
		if err != nil {
			return nil, errors.Wrap(err, "could not create custom provider client")
		}
		client.providers = append(client.providers, provider)
	}

	if providerOptions.Gotify != nil && (len(options.Providers) == 0 || sliceutil.Contains(options.Providers, "gotify")) {

		provider, err := gotify.New(providerOptions.Gotify, options.IDs)
		if err != nil {
			return nil, errors.Wrap(err, "could not create gotify provider client")
		}
		client.providers = append(client.providers, provider)
	}

	if providerOptions.RocketChat != nil && (len(options.Providers) == 0 || sliceutil.Contains(options.Providers, "rocketchat")) {

		provider, err := rocketchat.New(providerOptions.RocketChat, options.IDs)
		if err != nil {
			return nil, errors.Wrap(err, "could not create rocketchat provider client")
		}
		client.providers = append(client.providers, provider)
	}

	return client, nil
}

// SendResult is the per-channel outcome of one SendWithResults call.
type SendResult struct {
	// Provider is the stable channel name (e.g. "slack", "discord").
	Provider string
	Err      error
}

// Names returns the stable channel names of the configured providers, in
// send order. The names are derived from the provider type and contain no
// credentials, timestamps or formatting details, so they are safe to use
// as dedup channel identifiers.
func (p *Client) Names() []string {
	names := make([]string, 0, len(p.providers))
	for _, v := range p.providers {
		names = append(names, providerName(v))
	}
	return names
}

// providerName returns the package name of a provider implementation,
// e.g. "*slack.Provider" -> "slack".
func providerName(v Provider) string {
	t := fmt.Sprintf("%T", v) // e.g. *slack.Provider
	t = strings.TrimPrefix(t, "*")
	if i := strings.IndexByte(t, '.'); i >= 0 {
		t = t[:i]
	}
	return t
}

// SendWithResults sends the message to each configured provider and
// reports the outcome per channel. When only is non-empty, providers whose
// name is not in the list are skipped. A failure in one channel never
// affects the others.
func (p *Client) SendWithResults(message string, only []string) []SendResult {
	// strip unsupported color control chars
	message = stripansi.Strip(message)

	var filter map[string]bool
	if len(only) > 0 {
		filter = make(map[string]bool, len(only))
		for _, name := range only {
			filter[name] = true
		}
	}

	results := make([]SendResult, 0, len(p.providers))
	for _, v := range p.providers {
		name := providerName(v)
		if filter != nil && !filter[name] {
			continue
		}
		results = append(results, SendResult{Provider: name, Err: v.Send(message, p.options.MessageFormat)})
	}
	return results
}

func (p *Client) Send(message string) error {
	for _, result := range p.SendWithResults(message, nil) {
		if result.Err != nil {
			for _, v := range multierr.Errors(result.Err) {
				gologger.Error().Msgf("%s", v)
			}
		}
	}
	return nil
}
