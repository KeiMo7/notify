package providers

import (
	"github.com/acarl005/stripansi"
	"github.com/pkg/errors"
	"go.uber.org/multierr"

	"github.com/projectdiscovery/gologger"
	msgpkg "github.com/projectdiscovery/notify/pkg/message"
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

// ChunkProvider is implemented by every channel. A channel owns its
// character budget and markup grammar: Prepare turns a redacted raw message
// into an ordered chunk sequence, and SendChunks delivers every chunk in
// order. A failure is scoped to the implementing channel: the client keeps
// dispatching to the remaining channels.
type ChunkProvider interface {
	// Name identifies the channel type for channel-scoped error messages.
	Name() string
	// Validate rejects illegal channel budgets at startup, before any
	// message is ever sent.
	Validate() error
	// Prepare applies the channel's syntax escaping and budgeted splitting
	// to an already-redacted message. A blank message yields nil chunks.
	Prepare(redacted string) ([]msgpkg.Chunk, error)
	// SendChunks delivers chunks in their given order, applying the user
	// format string to each chunk body.
	SendChunks(chunks []msgpkg.Chunk, cliFormat string) error
}

// Provider is the canonical channel interface; the alias keeps the original
// name available to external wiring and tests.
type Provider = ChunkProvider

type Client struct {
	providers       []ChunkProvider
	providerOptions *ProviderOptions
	options         *types.Options
}

// NewFromClients builds a client from pre-constructed channel clients. It
// exists for tests and for embedders that wire channels programmatically
// instead of through the YAML provider config. Illegal budgets are rejected
// here so misconfiguration fails at startup.
func NewFromClients(providers []ChunkProvider, options *types.Options) (*Client, error) {
	for _, p := range providers {
		if err := p.Validate(); err != nil {
			return nil, errors.Wrapf(err, "invalid channel configuration for %s", p.Name())
		}
	}
	if options == nil {
		options = &types.Options{}
	}
	return &Client{providers: providers, options: options}, nil
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
		if nil != err {
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

// Send applies the channel-independent secrecy policy once, then dispatches
// per channel. A failure on one channel never blocks the others: every
// error is logged with the responsible channel name so operators can locate
// the failing destination, while Send keeps delivering to the remaining
// channels and returns nil (a failed channel must not abort the run).
func (p *Client) Send(message string) error {

	// strip unsupported color control chars
	message = stripansi.Strip(message)

	// Shared redaction: every channel and every log line sees the masked
	// text, never the cleartext secret.
	message = msgpkg.Redact(message)

	for _, v := range p.providers {
		chunks, err := v.Prepare(message)
		if err != nil {
			gologger.Error().Msgf("channel %s: failed to prepare message: %s", v.Name(), err)
			continue
		}
		if len(chunks) == 0 {
			// Blank message: nothing to deliver to this channel.
			continue
		}
		if err := v.SendChunks(chunks, p.options.MessageFormat); err != nil {
			// Keep the failure scoped to this channel: log each concrete
			// cause (already channel- and chunk-tagged by the provider) and
			// continue with the remaining channels.
			for _, e := range multierr.Errors(err) {
				gologger.Error().Msgf("channel %s: %s", v.Name(), e)
			}
		}
	}

	return nil
}
