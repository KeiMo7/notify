package smtp

import (
	"fmt"
	"net/url"
	"strconv"
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
	SMTP    []*Options `yaml:"smtp,omitempty"`
	counter int
}

type Options struct {
	ID              string   `yaml:"id,omitempty"`
	Server          string   `yaml:"smtp_server,omitempty"`
	Username        string   `yaml:"smtp_username,omitempty"`
	Password        string   `yaml:"smtp_password,omitempty"`
	FromAddress     string   `yaml:"from_address,omitempty"`
	SMTPCC          []string `yaml:"smtp_cc,omitempty"`
	SMTPFormat      string   `yaml:"smtp_format,omitempty"`
	Subject         string   `yaml:"subject,omitempty"`
	HTML            bool     `yaml:"smtp_html,omitempty"`
	DisableStartTLS bool     `yaml:"smtp_disable_starttls,omitempty"`
	// MessageLimit overrides the per-channel character budget (runes).
	MessageLimit int `yaml:"message_limit,omitempty"`
}

func New(options []*Options, ids []string) (*Provider, error) {
	provider := &Provider{}

	for _, o := range options {
		if len(ids) == 0 || sliceutil.Contains(ids, o.ID) {
			provider.SMTP = append(provider.SMTP, o)
		}
	}

	provider.counter = 0

	if err := provider.Validate(); err != nil {
		return nil, err
	}
	return provider, nil
}

func (p *Provider) Name() string { return "smtp" }

func (p *Provider) Validate() error {
	for _, pr := range p.SMTP {
		if pr.MessageLimit != 0 {
			if err := msgpkg.ValidateBudget(pr.MessageLimit); err != nil {
				return errors.Wrapf(err, "smtp id %q", pr.ID)
			}
		}
	}
	return nil
}

func (p *Provider) budget() int {
	budget := msgpkg.DefaultSMTPBudget
	for _, pr := range p.SMTP {
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
	return msgpkg.DefaultSMTPBudget
}

func (p *Provider) Prepare(redacted string) ([]msgpkg.Chunk, error) {
	return msgpkg.Process(redacted, msgpkg.Profile{Budget: p.budget(), Syntax: msgpkg.SyntaxNone})
}

func (p *Provider) SendChunks(chunks []msgpkg.Chunk, cliFormat string) error {
	var smtpErr error
	p.counter++
	for _, pr := range p.SMTP {
		// Each chunk becomes its own email with a numbered subject so the
		// receiver can reassemble in order even if delivery is retried.
		perEndpoint, err := msgpkg.Refit(chunks, p.endpointBudget(pr))
		if err != nil {
			smtpErr = multierr.Append(smtpErr, errors.Wrapf(err, "id: %s", pr.ID))
			continue
		}
		for _, chunk := range perEndpoint {
			body := utils.FormatMessage(chunk.Text, utils.SelectFormat(cliFormat, pr.SMTPFormat), p.counter)
			msg := msgpkg.RenderChunk(msgpkg.Chunk{Index: chunk.Index, Total: chunk.Total, Text: body})
			destination := buildUrl(pr, chunk.Index, chunk.Total)
			if err := shoutrrr.Send(destination, msg); err != nil {
				smtpErr = multierr.Append(smtpErr, errors.Wrapf(err,
					"failed to send smtp notification for id: %s chunk %d/%d", pr.ID, chunk.Index, chunk.Total))
				break
			}
		}
		gologger.Verbose().Msgf("smtp notification sent for id: %s", pr.ID)
	}
	return smtpErr
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

// buildUrl keeps the original URL shape (the existing test parses it) but
// appends chunk numbering to the subject when the message was split.
func buildUrl(o *Options, index, total int) string {
	subject := o.Subject
	if total > 1 {
		subject = fmt.Sprintf("%s [%d/%d]", subject, index, total)
	}
	return fmt.Sprintf("smtp://%s:%s@%s/?fromAddress=%s&toAddresses=%s&subject=%s&UseHTML=%s&UseStartTLS=%s",
		o.Username, url.QueryEscape(o.Password), o.Server, o.FromAddress,
		strings.Join(o.SMTPCC, ","), subject,
		strconv.FormatBool(o.HTML), strconv.FormatBool(!o.DisableStartTLS))
}
