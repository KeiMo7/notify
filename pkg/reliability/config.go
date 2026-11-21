// Package reliability adds opt-in failure handling on top of the existing
// provider adapters: failure classification, bounded backoff retries,
// per-provider circuit breakers (degradation), duplicate suppression and a
// local spill queue for the case where every provider is unavailable.
//
// Every capability is dormant unless Config.Enabled is true, so the legacy
// send path keeps its exact behavior.
package reliability

import (
	"errors"
	"time"
)

// FailureClass is the category assigned to a single send attempt.
type FailureClass int

const (
	// ClassOK means the attempt succeeded (no failure to classify).
	ClassOK FailureClass = iota
	// ClassRateLimited means the channel explicitly asked us to slow down.
	ClassRateLimited
	// ClassAuthFailed means credentials were rejected; retrying cannot help.
	ClassAuthFailed
	// ClassContentRejected means the message payload was refused.
	ClassContentRejected
	// ClassUnreachable means the target could not be reached or answered
	// with a transient server-side failure.
	ClassUnreachable
	// ClassUnknown is the bucket for anything we cannot attribute.
	ClassUnknown
)

// String renders the class for logs and the status snapshot.
func (c FailureClass) String() string {
	switch c {
	case ClassOK:
		return "ok"
	case ClassRateLimited:
		return "rate_limited"
	case ClassAuthFailed:
		return "auth_failed"
	case ClassContentRejected:
		return "content_rejected"
	case ClassUnreachable:
		return "unreachable"
	default:
		return "unknown"
	}
}

// Retryable reports whether an attempt of this class is eligible for retry.
func (c FailureClass) Retryable() bool {
	switch c {
	case ClassRateLimited, ClassUnreachable, ClassUnknown:
		return true
	default:
		// Auth and content rejection are deterministic failures.
		return false
	}
}

// Config is the reliability section of the provider config. All fields are
// optional; zero value means the feature is disabled and every other field
// falls back to the documented default.
type Config struct {
	// Enabled gates the whole feature. When false, none of the other
	// fields have any effect and the legacy send path is untouched.
	Enabled bool `yaml:"enabled,omitempty"`

	// MaxRetries bounds the number of *retries* (the original attempt is
	// not counted) performed for a single provider send.
	MaxRetries int `yaml:"max_retries,omitempty"`
	// BackoffInitial is the delay before the first retry. Subsequent
	// delays grow exponentially (BackoffInitial * 2^attempt) up to
	// BackoffMax, and are shortened by an explicit Retry-After signal.
	BackoffInitial time.Duration `yaml:"backoff_initial,omitempty"`
	// BackoffMax caps the exponential growth of a single retry delay.
	BackoffMax time.Duration `yaml:"backoff_max,omitempty"`
	// MaxRetryTime caps the wall-clock time spent retrying a single
	// provider send. Together with MaxRetries this bounds total push time.
	MaxRetryTime time.Duration `yaml:"max_retry_time,omitempty"`

	// FailureThreshold is the number of consecutive failed sends (across
	// all classes) after which a provider is paused (circuit opens).
	FailureThreshold int `yaml:"failure_threshold,omitempty"`
	// Cooldown is how long a provider stays paused before a probe is
	// allowed through to test recovery.
	Cooldown time.Duration `yaml:"cooldown,omitempty"`

	// DedupWindow: identical messages arriving within this window are
	// merged into a single summary push. Zero disables dedup.
	DedupWindow time.Duration `yaml:"dedup_window,omitempty"`

	// QueuePath is the file backing the local spill queue. Empty means
	// the platform default temp directory is used.
	QueuePath string `yaml:"queue_path,omitempty"`
	// QueueMaxEntries bounds the on-disk queue; when full the oldest
	// entries are dropped and counted.
	QueueMaxEntries int `yaml:"queue_max_entries,omitempty"`
}

// Default values applied when the feature is enabled but a field is omitted.
const (
	defaultMaxRetries       = 3
	defaultBackoffInitial   = 500 * time.Millisecond
	defaultBackoffMax       = 10 * time.Second
	defaultMaxRetryTime     = 15 * time.Second
	defaultFailureThreshold = 5
	defaultCooldown         = 60 * time.Second
	defaultDedupWindow      = 0 // dedup is opt-in even inside the enabled feature
	defaultQueueMaxEntries  = 1000
)

// Defaults returns a copy of cfg with zero fields replaced by defaults.
// It is the exported counterpart used by the wiring layer.
func (cfg Config) Defaults() Config {
	return cfg.withDefaults()
}

// withDefaults returns a copy of cfg with zero fields replaced by defaults.
func (cfg Config) withDefaults() Config {
	out := cfg
	if out.MaxRetries == 0 {
		out.MaxRetries = defaultMaxRetries
	}
	if out.BackoffInitial == 0 {
		out.BackoffInitial = defaultBackoffInitial
	}
	if out.BackoffMax == 0 {
		out.BackoffMax = defaultBackoffMax
	}
	if out.MaxRetryTime == 0 {
		out.MaxRetryTime = defaultMaxRetryTime
	}
	if out.FailureThreshold == 0 {
		out.FailureThreshold = defaultFailureThreshold
	}
	if out.Cooldown == 0 {
		out.Cooldown = defaultCooldown
	}
	if out.QueueMaxEntries == 0 {
		out.QueueMaxEntries = defaultQueueMaxEntries
	}
	return out
}

// Validate rejects illegal configuration at startup with a message naming
// the offending field. It must run before defaults are applied, so that an
// explicit negative value is never silently rewritten.
func (cfg Config) Validate() error {
	if !cfg.Enabled {
		return nil
	}
	if cfg.MaxRetries < 0 {
		return errors.New("reliability.max_retries must be >= 0")
	}
	if cfg.BackoffInitial < 0 {
		return errors.New("reliability.backoff_initial must be >= 0")
	}
	if cfg.BackoffMax < 0 {
		return errors.New("reliability.backoff_max must be >= 0")
	}
	if cfg.MaxRetryTime < 0 {
		return errors.New("reliability.max_retry_time must be >= 0")
	}
	if cfg.FailureThreshold < 0 {
		return errors.New("reliability.failure_threshold must be >= 0")
	}
	if cfg.Cooldown < 0 {
		return errors.New("reliability.cooldown must be >= 0")
	}
	if cfg.DedupWindow < 0 {
		return errors.New("reliability.dedup_window must be >= 0")
	}
	if cfg.QueueMaxEntries < 0 {
		return errors.New("reliability.queue_max_entries must be >= 0")
	}
	if cfg.BackoffInitial > 0 && cfg.BackoffMax > 0 && cfg.BackoffInitial > cfg.BackoffMax {
		return errors.New("reliability.backoff_initial must not exceed reliability.backoff_max")
	}
	if cfg.MaxRetryTime > 0 && cfg.BackoffInitial > cfg.MaxRetryTime {
		return errors.New("reliability.backoff_initial must not exceed reliability.max_retry_time")
	}
	return nil
}
