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

	Dedup              bool   `yaml:"dedup,omitempty"`
	DedupStore         string `yaml:"dedup_store,omitempty"`
	DedupRetention     int    `yaml:"dedup_retention,omitempty"`
	DedupKeyRegex      string `yaml:"dedup_key_regex,omitempty"`
	DedupReportRemoved bool   `yaml:"dedup_report_removed,omitempty"`
}
