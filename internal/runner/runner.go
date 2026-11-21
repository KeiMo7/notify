package runner

import (
	"bufio"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/containrrr/shoutrrr"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v3"

	"github.com/projectdiscovery/gologger"
	"github.com/projectdiscovery/notify/pkg/providers"
	"github.com/projectdiscovery/notify/pkg/reliability"
	"github.com/projectdiscovery/notify/pkg/types"
	"github.com/projectdiscovery/notify/pkg/utils"
	fileutil "github.com/projectdiscovery/utils/file"
)

// Runner contains the internal logic of the program
type Runner struct {
	options   *types.Options
	providers *providers.Client
}

// NewRunner instance
func NewRunner(options *types.Options) (*Runner, error) {
	var providerOptions providers.ProviderOptions

	if options.ProviderConfig == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		options.ProviderConfig = filepath.Join(home, types.DefaultProviderConfigLocation)
		gologger.Print().Msgf("Using default provider config: %s\n", options.ProviderConfig)
	}

	reader, err := fileutil.SubstituteConfigFromEnvVars(options.ProviderConfig)
	if err != nil {
		return nil, err
	}

	if parseErr := yaml.NewDecoder(reader).Decode(&providerOptions); parseErr != nil {
		return nil, errors.Wrap(parseErr, "could not parse provider config file")
	}

	// The CLI flag can switch the feature on even when the provider config
	// omits the section entirely; parameters then fall back to defaults.
	// The YAML section stays the only place to tune individual parameters,
	// and an explicitly disabled section is never overridden back on.
	if options.ReliabilityEnabled && !providerOptions.Reliability.Enabled {
		providerOptions.Reliability.Enabled = true
	}

	shoutrrr.SetLogger(log.New(io.Discard, "", 0))

	prClient, err := providers.New(&providerOptions, options)
	if err != nil {
		return nil, err
	}

	return &Runner{options: options, providers: prClient}, nil
}

// Run polling and notification
func (r *Runner) Run() error {
	defaultTransport := http.DefaultTransport.(*http.Transport)
	if r.options.Proxy != "" {
		proxyurl, err := url.Parse(r.options.Proxy)
		if err != nil || proxyurl == nil {
			gologger.Warning().Msgf("supplied proxy '%s' is not valid", r.options.Proxy)
		} else {
			// keep certificate verification: provider requests carry tokens
			defaultTransport = defaultTransport.Clone()
			defaultTransport.Proxy = http.ProxyURL(proxyurl)
		}
	}

	http.DefaultClient.Transport = defaultTransport
	if r.options.RateLimit > 0 {
		http.DefaultClient.Transport = utils.NewThrottledTransport(time.Second, r.options.RateLimit, defaultTransport)
	}

	// When reliability is enabled, wrap the transport actually in use so the
	// classifier observes explicit channel signals (HTTP 429 / Retry-After).
	// The wrapper is pass-through: request and body bytes are never copied or
	// buffered, so a non-reliability run is byte-for-byte the old path.
	var engine *reliability.Engine
	if r.providers != nil {
		engine = r.providers.ReliabilityEngine()
	}
	if engine != nil {
		http.DefaultClient.Transport = reliability.NewInstrumentedTransport(
			http.DefaultClient.Transport, engine.Collector())

		// Replay anything left in the spill queue by a previous run that
		// ended while all channels were unavailable.
		if replayed, remaining := engine.ReplayQueue(); replayed > 0 {
			gologger.Info().Msgf("reliability: replayed %d queued message(s), %d still pending", replayed, remaining)
		}
	}

	var inFile *os.File
	var err error
	var splitter bufio.SplitFunc

	switch {
	case r.options.Data != "":
		inFile, err = os.Open(r.options.Data)
		if err != nil {
			gologger.Fatal().Msgf("%s\n", err)
		}
	case fileutil.HasStdin():
		inFile = os.Stdin
	default:
		return errors.New("notify works with stdin or file using -data flag")
	}

	br := bufio.NewScanner(inFile)

	if r.options.CharLimit > bufio.MaxScanTokenSize {
		// Satisfy the condition of our splitters, which is that charLimit is <= the size of the bufio.Scanner buffer
		buffer := make([]byte, 0, r.options.CharLimit)
		br.Buffer(buffer, r.options.CharLimit)
	}

	if r.options.Bulk {
		splitter, err = bulkSplitter(r.options.CharLimit)
	} else {
		splitter, err = lineLengthSplitter(r.options.CharLimit)
	}

	if err != nil {
		return err
	}

	br.Split(splitter)

	// Flush any coalesced duplicate window at normal end of input. Close is
	// idempotent-ish: an empty deduper emits nothing, and the signal handler
	// may also call it on termination without harm.
	defer r.Close()

	for br.Scan() {
		msg := br.Text()
		//nolint:errcheck
		r.sendMessage(msg)
	}
	return br.Err()
}

func (r *Runner) sendMessage(msg string) error {
	if len(msg) > 0 {
		if r.options.Delay > 0 {
			time.Sleep(time.Duration(r.options.Delay) * time.Second)
		}
		gologger.Silent().Msgf("%s\n", msg)
		err := r.providers.Send(msg)
		if err != nil {
			return err
		}
	}
	return nil
}

// Close the runner instance
func (r *Runner) Close() {
	if r.providers != nil {
		if engine := r.providers.ReliabilityEngine(); engine != nil {
			engine.Close()
		}
	}
}
