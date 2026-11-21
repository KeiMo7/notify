package providers

import (
	"os"
	"path/filepath"

	"github.com/pkg/errors"

	"github.com/projectdiscovery/notify/pkg/reliability"
	"github.com/projectdiscovery/notify/pkg/types"
)

// initReliability wraps every configured adapter with the reliability
// machinery. The adapters themselves are not modified: wrapping is the only
// touch point, and when the feature is disabled the function is a no-op so
// the legacy send path (including its exact call sequence) is preserved.
func (p *Client) initReliability(cfg *reliability.Config) error {
	if cfg == nil || !cfg.Enabled {
		return nil
	}

	resolved := cfg.Defaults()

	queuePath := resolved.QueuePath
	if queuePath == "" {
		queuePath = defaultQueuePath()
	}
	queue, err := reliability.OpenLocalQueue(queuePath, resolved.QueueMaxEntries)
	if err != nil {
		return errors.Wrap(err, "could not open reliability spill queue")
	}

	wrapped := make([]*reliability.ReliableProvider, 0, len(p.providers))
	for _, provider := range p.providers {
		// A nil collector is replaced by the engine's shared collector.
		rp := reliability.NewReliableProvider(providerName(provider), provider, resolved, nil)
		wrapped = append(wrapped, rp)
	}

	p.engine = reliability.NewEngine(reliability.EngineOptions{
		Config:        resolved,
		Providers:     wrapped,
		Queue:         queue,
		MessageFormat: p.options.MessageFormat,
	})
	return nil
}

// defaultQueuePath keeps spill state next to the provider config so it
// survives restarts and is easy for operators to find and inspect.
func defaultQueuePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "notify-pending-queue.json")
	}
	return filepath.Join(home, types.DefaultProviderConfigLocation+".pending.json")
}

// NewForTest builds a Client with an explicit provider list, bypassing the
// YAML-driven adapter construction. It exists for tests that need to drive
// the full wiring (including the reliability engine) against a stub channel.
func NewForTest(po *ProviderOptions, options *types.Options, prs ...Provider) (*Client, error) {
	client := &Client{providers: prs, providerOptions: po, options: options}
	if err := po.Reliability.Validate(); err != nil {
		return nil, errors.Wrap(err, "invalid reliability configuration")
	}
	if err := client.initReliability(&po.Reliability); err != nil {
		return nil, err
	}
	return client, nil
}
