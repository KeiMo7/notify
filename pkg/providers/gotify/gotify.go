package gotify

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
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
	Gotify  []*Options `yaml:"gotify,omitempty"`
	counter int
}

type Options struct {
	ID               string `yaml:"id,omitempty"`
	GotifyHost       string `yaml:"gotify_host,omitempty"`
	GotifyPort       string `yaml:"gotify_port,omitempty"`
	GotifyToken      string `yaml:"gotify_token,omitempty"`
	GotifyFormat     string `yaml:"gotify_format,omitempty"`
	GotifyDisableTLS bool   `yaml:"gotify_disabletls,omitempty"`
	GotifyTitle      string `yaml:"gotify_title,omitempty"`
	// MessageLimit overrides the per-channel character budget (runes).
	MessageLimit int `yaml:"message_limit,omitempty"`
}

func New(options []*Options, ids []string) (*Provider, error) {
	provider := &Provider{}

	for _, o := range options {
		if len(ids) == 0 || sliceutil.Contains(ids, o.ID) {
			provider.Gotify = append(provider.Gotify, o)
		}
	}

	provider.counter = 0

	if err := provider.Validate(); err != nil {
		return nil, err
	}
	return provider, nil
}

func (p *Provider) Name() string { return "gotify" }

func (p *Provider) Validate() error {
	for _, pr := range p.Gotify {
		if pr.MessageLimit != 0 {
			if err := msgpkg.ValidateBudget(pr.MessageLimit); err != nil {
				return errors.Wrapf(err, "gotify id %q", pr.ID)
			}
		}
	}
	return nil
}

func (p *Provider) budget() int {
	budget := msgpkg.DefaultGotifyBudget
	for _, pr := range p.Gotify {
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
	return msgpkg.DefaultGotifyBudget
}

func (p *Provider) Prepare(redacted string) ([]msgpkg.Chunk, error) {
	return msgpkg.Process(redacted, msgpkg.Profile{Budget: p.budget(), Syntax: msgpkg.SyntaxNone})
}

func (p *Provider) SendChunks(chunks []msgpkg.Chunk, cliFormat string) error {
	var gotifyErr error
	p.counter++

	for _, pr := range p.Gotify {
		params := url.Values{}
		if pr.GotifyTitle != "" {
			params.Add("title", pr.GotifyTitle)
		}
		if pr.GotifyDisableTLS {
			params.Add("disabletls", "true")
		}
		perEndpoint, err := msgpkg.Refit(chunks, p.endpointBudget(pr))
		if err != nil {
			gotifyErr = multierr.Append(gotifyErr, errors.Wrapf(err, "id: %s", pr.ID))
			continue
		}
		for _, chunk := range perEndpoint {
			body := utils.FormatMessage(chunk.Text, utils.SelectFormat(cliFormat, pr.GotifyFormat), p.counter)
			msg := msgpkg.RenderChunk(msgpkg.Chunk{Index: chunk.Index, Total: chunk.Total, Text: body})

			var err error
			if isLegacyGotifyToken(pr.GotifyToken) {
				dest := fmt.Sprintf("gotify://%s:%s/%s", pr.GotifyHost, pr.GotifyPort, pr.GotifyToken)
				if len(params) > 0 {
					dest += "?" + params.Encode()
				}
				err = shoutrrr.Send(dest, msg)
				if err != nil && strings.Contains(err.Error(), "invalid gotify token") {
					err = sendGotifyDirect(pr, msg)
				}
			} else {
				// Gotify 3 tokens (e.g. gtfya.*) are rejected by shoutrrr v0.8.0 which expects legacy 15-char tokens starting with 'A'.
				// Direct REST API POST sends directly to the Gotify server.
				err = sendGotifyDirect(pr, msg)
			}

			if err != nil {
				gotifyErr = multierr.Append(gotifyErr, errors.Wrapf(err,
					"failed to send gotify notification for id: %s chunk %d/%d", pr.ID, chunk.Index, chunk.Total))
				break
			}
		}
		gologger.Verbose().Msgf("gotify notification sent for id: %s", pr.ID)
	}

	return gotifyErr
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

type gotifyPayload struct {
	Message  string `json:"message"`
	Title    string `json:"title,omitempty"`
	Priority int    `json:"priority,omitempty"`
}

// gotifyCheckRedirect follows redirects on the configured host, including
// HTTP to HTTPS, and stops before the Gotify token is sent to another host.
func gotifyCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 || req.URL == nil || via[0].URL == nil {
		return nil
	}
	if !strings.EqualFold(req.URL.Hostname(), via[0].URL.Hostname()) {
		return http.ErrUseLastResponse
	}
	return nil
}

var gotifyHTTPClient = &http.Client{
	Timeout:       10 * time.Second,
	CheckRedirect: gotifyCheckRedirect,
}

func isLegacyGotifyToken(token string) bool {
	if len(token) != 15 || token[0] != 'A' {
		return false
	}
	const tokenChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.-_"
	for _, c := range token {
		if !strings.ContainsRune(tokenChars, c) {
			return false
		}
	}
	return true
}

func sendGotifyDirect(pr *Options, msg string) error {
	scheme := "https"
	if pr.GotifyDisableTLS {
		scheme = "http"
	}
	hostPort := pr.GotifyHost
	if pr.GotifyPort != "" {
		hostPort = net.JoinHostPort(pr.GotifyHost, pr.GotifyPort)
	}
	endpoint := fmt.Sprintf("%s://%s/message", scheme, hostPort)

	payload := gotifyPayload{
		Message: msg,
		Title:   pr.GotifyTitle,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return errors.Wrap(err, "failed to marshal gotify payload")
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.Wrap(err, "failed to create gotify request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gotify-Key", pr.GotifyToken)

	client := gotifyHTTPClient
	if pr.GotifyDisableTLS {
		client = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
			Timeout:       10 * time.Second,
			CheckRedirect: gotifyCheckRedirect,
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return errors.Wrap(err, "failed to send gotify request")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("gotify server returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	// Drain the success body so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
