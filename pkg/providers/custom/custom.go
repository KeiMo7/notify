package custom

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig"
	"github.com/pkg/errors"
	"go.uber.org/multierr"

	"github.com/projectdiscovery/gologger"
	msgpkg "github.com/projectdiscovery/notify/pkg/message"
	"github.com/projectdiscovery/notify/pkg/utils"
	"github.com/projectdiscovery/notify/pkg/utils/httpreq"
	sliceutil "github.com/projectdiscovery/utils/slice"
)

type Provider struct {
	Custom  []*Options `yaml:"custom,omitempty"`
	counter int
}

type Options struct {
	ID               string            `yaml:"id,omitempty"`
	CustomWebhookURL string            `yaml:"custom_webhook_url,omitempty"`
	CustomMethod     string            `yaml:"custom_method,omitempty"`
	CustomHeaders    map[string]string `yaml:"custom_headers,omitempty"`
	CustomFormat     string            `yaml:"custom_format,omitempty"`
	CustomSprig      string            `yaml:"custom_sprig,omitempty"`
	// MessageLimit overrides the per-channel character budget (runes).
	MessageLimit int `yaml:"message_limit,omitempty"`
}

func New(options []*Options, ids []string) (*Provider, error) {
	provider := &Provider{}

	for _, o := range options {
		if len(ids) == 0 || sliceutil.Contains(ids, o.ID) {
			provider.Custom = append(provider.Custom, o)
		}
	}

	provider.counter = 0

	if err := provider.Validate(); err != nil {
		return nil, err
	}
	return provider, nil
}

func (p *Provider) Name() string { return "custom" }

func (p *Provider) Validate() error {
	for _, pr := range p.Custom {
		if pr.MessageLimit != 0 {
			if err := msgpkg.ValidateBudget(pr.MessageLimit); err != nil {
				return errors.Wrapf(err, "custom id %q", pr.ID)
			}
		}
	}
	return nil
}

// budget returns the largest configured endpoint budget: Prepare produces
// boundary-aligned base fragments for it, and SendChunks shrinks them per
// endpoint with message.Refit when an endpoint has a smaller limit.
func (p *Provider) budget() int {
	budget := msgpkg.DefaultCustomBudget
	for _, pr := range p.Custom {
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
	return msgpkg.DefaultCustomBudget
}

// Prepare redaction has already run in the client. For sprig endpoints the
// input is a JSON document that must stay whole to remain parseable, so the
// template is rendered first and only its output is budgeted/split.
func (p *Provider) Prepare(redacted string) ([]msgpkg.Chunk, error) {
	if msgpkg.IsBlank(redacted) {
		return nil, nil
	}
	sprigOutput := ""
	hasSprig := false
	for _, pr := range p.Custom {
		if pr.CustomSprig != "" {
			hasSprig = true
			out, err := renderSprig(pr, redacted)
			if err != nil {
				return nil, err
			}
			sprigOutput = out
			break
		}
	}
	if hasSprig {
		return msgpkg.Split(sprigOutput, p.budget())
	}
	return msgpkg.Process(redacted, msgpkg.Profile{Budget: p.budget(), Syntax: msgpkg.SyntaxNone})
}

func renderSprig(pr *Options, message string) (string, error) {
	// Convert a string to JSON
	data := make(map[string]interface{})
	if err := json.Unmarshal([]byte(message), &data); err != nil {
		return "", errors.Wrap(err, fmt.Sprintf("failed to unmarshal message to JSON for id: %s ", pr.ID))
	}

	funcMap := sprig.TxtFuncMap()
	tmpl, err := template.New("sprig").Funcs(funcMap).Parse(pr.CustomSprig)
	if err != nil {
		return "", errors.Wrap(err, fmt.Sprintf("failed to parse custom sprig template for id: %s", pr.ID))
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", errors.Wrap(err, fmt.Sprintf("failed to execute custom sprig template for id: %s", pr.ID))
	}
	return buf.String(), nil
}

func (p *Provider) SendChunks(chunks []msgpkg.Chunk, cliFormat string) error {
	var customErr error
	p.counter++

	for _, pr := range p.Custom {
		// Shrink the shared base fragments to this endpoint's own budget.
		perEndpoint, err := msgpkg.Refit(chunks, p.endpointBudget(pr))
		if err != nil {
			customErr = multierr.Append(customErr, errors.Wrap(err, fmt.Sprintf("failed to budget chunks for id: %s", pr.ID)))
			continue
		}
		for _, chunk := range perEndpoint {
			msg, err := p.renderChunk(pr, chunk, cliFormat)
			if err != nil {
				customErr = multierr.Append(customErr, err)
				break
			}

			body := bytes.NewBufferString(msg)

			r, err := http.NewRequest(pr.CustomMethod, pr.CustomWebhookURL, body)
			if err != nil {
				customErr = multierr.Append(customErr, errors.Wrap(err, fmt.Sprintf("failed to send custom notification for id: %s", pr.ID)))
				break
			}

			for k, v := range pr.CustomHeaders {
				r.Header.Set(k, v)
			}

			resp, err := httpreq.NewClient().Do(r)
			if err != nil {
				customErr = multierr.Append(customErr, errors.Wrap(err, fmt.Sprintf("failed to send custom notification for id: %s", pr.ID)))
				break
			}
			if resp != nil {
				_ = resp.Body.Close()
				if resp.StatusCode >= 400 {
					customErr = multierr.Append(customErr, fmt.Errorf("custom id %s: received non-success status: %s", pr.ID, resp.Status))
					break
				}
			}
			gologger.Verbose().Msgf("custom notification sent for id: %s (length %d chars)", pr.ID, msgpkg.RuneLen(msg))
		}
	}
	return customErr
}

// renderChunk applies the endpoint format. The message has already been
// redacted upstream, so only redacted text can reach the wire or the log.
func (p *Provider) renderChunk(pr *Options, chunk msgpkg.Chunk, cliFormat string) (string, error) {
	body := chunk.Text
	if pr.CustomSprig == "" {
		if strings.Contains(pr.CustomFormat, "{{dataJsonString}}") {
			// Escape the chunk body to a JSON string.
			b, err := json.Marshal(body)
			if err != nil {
				return "", errors.Wrap(err, fmt.Sprintf("failed to escape message to JSON for id: %s", pr.ID))
			}
			body = strings.ReplaceAll(pr.CustomFormat, "{{dataJsonString}}", string(b))
		} else {
			body = utils.FormatMessage(body, utils.SelectFormat(cliFormat, pr.CustomFormat), p.counter)
		}
	}
	return msgpkg.RenderChunk(msgpkg.Chunk{Index: chunk.Index, Total: chunk.Total, Text: body}), nil
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
