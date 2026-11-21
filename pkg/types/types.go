package types

import "github.com/projectdiscovery/goflags"

type Options struct {
	Verbose        bool                `yaml:"verbose,omitempty"`
	NoColor        bool                `yaml:"no_color,omitempty"`
	Silent         bool                `yaml:"silent,omitempty"`
	Version        bool                `yaml:"version,omitempty"`
	ProviderConfig string              `yaml:"provider_config,omitempty"`
	Providers      goflags.StringSlice `yaml:"providers,omitempty"`
	IDs            goflags.StringSlice `yaml:"ids,omitempty"`
	Proxy          string              `yaml:"proxy,omitempty"`
	RateLimit      int                 `yaml:"rate_limit,omitempty"`
	Delay          int                 `yaml:"delay,omitempty"`

	MessageFormat string `yaml:"message_format,omitempty"`

	Stdin              bool
	Bulk               bool   `yaml:"bulk,omitempty"`
	CharLimit          int    `yaml:"char_limit,omitempty"`
	Data               string `yaml:"data,omitempty"`
	DisableUpdateCheck bool   `yaml:"disable_update_check,omitempty"`

	// ReliabilityEnabled is the CLI/config switch for the opt-in failure
	// handling feature (backoff retries, circuit breakers, dedup, spill
	// queue). The detailed parameters live in the provider config's
	// "reliability" section; this flag only turns the feature on.
	ReliabilityEnabled bool `yaml:"reliability,omitempty"`
}
