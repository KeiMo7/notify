package runner

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/containrrr/shoutrrr"
	"github.com/pkg/errors"
	"go.uber.org/multierr"
	"gopkg.in/yaml.v3"

	"github.com/projectdiscovery/gologger"
	"github.com/projectdiscovery/notify/pkg/dedup"
	"github.com/projectdiscovery/notify/pkg/providers"
	"github.com/projectdiscovery/notify/pkg/types"
	"github.com/projectdiscovery/notify/pkg/utils"
	fileutil "github.com/projectdiscovery/utils/file"
)

// providerClient is the subset of providers.Client used by the runner. It
// is an interface so tests can substitute fake channels.
type providerClient interface {
	Send(message string) error
	SendWithResults(message string, only []string) []providers.SendResult
	Names() []string
}

// Runner contains the internal logic of the program
type Runner struct {
	options   *types.Options
	providers providerClient
	deduper   *dedup.Store
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

	shoutrrr.SetLogger(log.New(io.Discard, "", 0))

	prClient, err := providers.New(&providerOptions, options)
	if err != nil {
		return nil, err
	}

	r := &Runner{options: options, providers: prClient}

	if options.Dedup {
		storePath := options.DedupStore
		if storePath == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			storePath = filepath.Join(home, types.DefaultDedupStoreLocation)
		}
		deduper, err := dedup.Open(dedup.Options{
			Path:      storePath,
			Retention: time.Duration(options.DedupRetention) * time.Hour,
			KeyRegex:  options.DedupKeyRegex,
		})
		if err != nil {
			// Corrupt or unreadable state: warn and degrade to a full push.
			gologger.Error().Msgf("%s", err)
		}
		// Probe writability up front; if the state directory is not
		// writable, warn and continue pushing without dedup.
		if err := deduper.Save(); err != nil {
			gologger.Error().Msgf("dedup state is not writable, continuing without deduplication: %s", err)
		} else {
			r.deduper = deduper
		}
	}

	return r, nil
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

	for br.Scan() {
		msg := br.Text()
		//nolint:errcheck
		r.sendMessage(msg)
	}
	if err := br.Err(); err != nil {
		_ = r.saveDedup()
		return err
	}
	if r.deduper != nil && r.options.DedupReportRemoved {
		r.reportRemoved()
	}
	return r.saveDedup()
}

// reportRemoved pushes one notification per entry that was present in the
// dedup state but absent from the current input batch, then drops it.
func (r *Runner) reportRemoved() {
	for _, entry := range r.deduper.Removed() {
		msg := fmt.Sprintf("[notify:removed] %s", entry.Content)
		gologger.Silent().Msgf("%s\n", msg)
		for _, res := range r.providers.SendWithResults(msg, nil) {
			if res.Err != nil {
				for _, e := range multierr.Errors(res.Err) {
					gologger.Error().Msgf("%s", e)
				}
				continue
			}
			r.deduper.Mark(entry.Content, res.Provider)
		}
		// The entry is dropped once every configured channel confirmed the
		// removal notice; otherwise it is retried on the next run.
		if len(r.deduper.PendingChannels(entry.Content, r.providers.Names())) == 0 {
			r.deduper.Drop(entry.Key)
		}
	}
}

// saveDedup persists the dedup state. A failure is logged and degrades the
// run to "no dedup next time" without failing the push itself.
func (r *Runner) saveDedup() error {
	if r.deduper == nil {
		return nil
	}
	if err := r.deduper.Save(); err != nil {
		gologger.Error().Msgf("could not save dedup state, next run will push without deduplication: %s", err)
		return err
	}
	return nil
}

func (r *Runner) sendMessage(msg string) error {
	if len(msg) == 0 {
		return nil
	}
	if r.options.Delay > 0 {
		time.Sleep(time.Duration(r.options.Delay) * time.Second)
	}
	gologger.Silent().Msgf("%s\n", msg)
	if r.deduper == nil {
		return r.providers.Send(msg)
	}
	return r.sendDeduped(msg)
}

// sendDeduped pushes msg only to the channels that have not yet received
// its current content, and records only confirmed successes.
func (r *Runner) sendDeduped(msg string) error {
	// Check first: for changed content it resets the per-channel records,
	// so PendingChannels below returns every channel again.
	decision := r.deduper.Check(msg)

	channels := r.providers.Names()
	pending := r.deduper.PendingChannels(msg, channels)
	if len(pending) == 0 {
		gologger.Verbose().Msgf("dedup: skipping already pushed message: %s", msg)
		return nil
	}

	out := msg
	if decision.Kind == dedup.ChangeContent {
		out = fmt.Sprintf("[notify:changed] %s\n- %s\n+ %s", decision.Key, decision.Previous, msg)
	}

	var sendErr error
	for _, res := range r.providers.SendWithResults(out, pending) {
		if res.Err != nil {
			// Failed channels are not marked, so the next run retries them.
			for _, e := range multierr.Errors(res.Err) {
				gologger.Error().Msgf("%s", e)
			}
			sendErr = res.Err
			continue
		}
		r.deduper.Mark(msg, res.Provider)
	}
	return sendErr
}

// Close the runner instance
func (r *Runner) Close() {
	//nolint:errcheck
	r.saveDedup()
}
