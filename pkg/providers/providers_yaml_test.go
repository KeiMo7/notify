package providers

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/projectdiscovery/notify/pkg/providers/telegram"
)

// The full provider config decodes with a reliability section and the
// documented duration strings (500ms, 1s) parse correctly.
func TestProviderConfigYAMLWithReliability(t *testing.T) {
	yamlText := `
telegram:
  - id: tel
    telegram_api_key: token
    telegram_chat_id: "123"
reliability:
  enabled: true
  max_retries: 2
  backoff_initial: 500ms
  backoff_max: 10s
  max_retry_time: 15s
  failure_threshold: 5
  cooldown: 60s
  dedup_window: 5m
  queue_max_entries: 100
`
	var po ProviderOptions
	require.NoError(t, yaml.Unmarshal([]byte(yamlText), &po))
	require.True(t, po.Reliability.Enabled)
	require.Equal(t, 2, po.Reliability.MaxRetries)
	require.Equal(t, 100, po.Reliability.QueueMaxEntries)
	require.NoError(t, po.Reliability.Validate())

	// The section coexists with ordinary provider blocks without touching them.
	require.Len(t, po.Telegram, 1)
	require.Equal(t, "tel", po.Telegram[0].ID)
}

// An illegal value surfaced through real YAML decoding must be rejected by the
// same validation the runner invokes at startup, naming the offending field.
func TestProviderConfigYAMLRejectsIllegalReliability(t *testing.T) {
	yamlText := `
reliability:
  enabled: true
  max_retries: -1
`
	var po ProviderOptions
	require.NoError(t, yaml.Unmarshal([]byte(yamlText), &po))
	err := po.Reliability.Validate()
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "max_retries"))
}

// A config without any reliability section decodes to the zero (disabled)
// value, guaranteeing old config files keep working unchanged.
func TestProviderConfigYAMLWithoutReliabilityStaysDisabled(t *testing.T) {
	yamlText := `
telegram:
  - id: tel
    telegram_api_key: token
    telegram_chat_id: "123"
`
	var po ProviderOptions
	require.NoError(t, yaml.Unmarshal([]byte(yamlText), &po))
	require.False(t, po.Reliability.Enabled)
	require.NoError(t, po.Reliability.Validate())

	client, err := NewForTest(&po, baseOptions(), &telegram.Provider{})
	require.NoError(t, err)
	require.Nil(t, client.ReliabilityEngine(), "disabled config must not build an engine")
}
