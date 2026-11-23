package rocketchat

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	msgpkg "github.com/projectdiscovery/notify/pkg/message"
)

func TestRocketChatSendWebhook(t *testing.T) {
	var receivedPayloads []webhookPayload

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))

		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		var p webhookPayload
		err = json.Unmarshal(body, &p)
		require.NoError(t, err)

		receivedPayloads = append(receivedPayloads, p)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer server.Close()

	opts := []*Options{
		{
			ID:                   "test-rc",
			RocketChatWebhookURL: server.URL,
			Channel:              "#security-alerts",
			Username:             "NotifyBot",
		},
	}

	provider, err := New(opts, []string{"test-rc"})
	require.NoError(t, err)
	require.NotNil(t, provider)

	err = provider.Send("Security alert: port 22 open", "")
	require.NoError(t, err)
	require.Len(t, receivedPayloads, 1)
	require.Equal(t, "Security alert: port 22 open", receivedPayloads[0].Text)
	require.Equal(t, "#security-alerts", receivedPayloads[0].Channel)
	require.Equal(t, "NotifyBot", receivedPayloads[0].Alias)
}

func TestRocketChatShoutrrrURLIncludesChannelAndUsername(t *testing.T) {
	got, err := rocketchatShoutrrrURL(&Options{
		RocketChatWebhookURL: "rocketchat://chat.example.com/tokenA/tokenB",
		Channel:              "#security-alerts",
		Username:             "NotifyBot",
	})
	require.NoError(t, err)
	require.Equal(t, "rocketchat://NotifyBot@chat.example.com/tokenA/tokenB/security-alerts", got)
}

func TestRocketChatPreservesNewlinesAndSequenceHeaders(t *testing.T) {
	var received []webhookPayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var p webhookPayload
		require.NoError(t, json.Unmarshal(body, &p))
		received = append(received, p)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	provider, err := New([]*Options{{
		ID: "nl", RocketChatWebhookURL: server.URL, MessageLimit: 50,
	}}, nil)
	require.NoError(t, err)

	msg := "first line 告警😀\nsecond line\n" + strings.Repeat("多行内容 ", 40)
	require.NoError(t, provider.Send(msg, ""))

	wantNewlines := strings.Count(msg, "\n")
	gotNewlines := 0
	for i, p := range received {
		gotNewlines += strings.Count(p.Text, "\n")
		require.Truef(t, strings.HasPrefix(p.Text, fmt.Sprintf("[%d/%d] ", i+1, len(received))),
			"fragment %d missing contiguous sequence header: %q", i+1, p.Text)
		require.True(t, utf8.ValidString(p.Text))
		require.LessOrEqual(t, utf8.RuneCountInString(p.Text), 50)
	}
	require.Equal(t, wantNewlines, gotNewlines, "newlines must survive splitting")
}

func TestRocketChatSplitMessage(t *testing.T) {
	part1 := strings.Repeat("A", 3000)
	part2 := strings.Repeat("B", 3000)
	fullMsg := part1 + "\n" + part2

	chunks, err := msgpkg.Split(fullMsg, 4000)
	require.NoError(t, err)
	require.Len(t, chunks, 2)
	// The newline stays attached to the preceding chunk so concatenating
	// the fragments reconstructs the original byte for byte.
	require.Equal(t, part1+"\n", chunks[0].Text)
	require.Equal(t, part2, chunks[1].Text)
	require.Equal(t, fullMsg, chunks[0].Text+chunks[1].Text)
	// Ordering metadata is present on every fragment.
	require.Equal(t, 1, chunks[0].Index)
	require.Equal(t, 2, chunks[0].Total)
	require.Equal(t, 2, chunks[1].Index)

	// A rune that straddles the byte limit must never be cut in half.
	// Breakable input: spaces give the splitter field boundaries while the
	// multi-byte runes sit inside ordinary words.
	breakable := strings.Repeat("A", 3900) + " café " + strings.Repeat("B", 20)
	chunks, err = msgpkg.Split(breakable, 4000)
	require.NoError(t, err)
	require.NotEmpty(t, chunks)
	for _, chunk := range chunks {
		require.True(t, utf8.ValidString(chunk.Text))
	}
	require.Contains(t, strings.Join([]string{chunks[0].Text, chunks[len(chunks)-1].Text}, ""), "é")
	require.Equal(t, breakable, joinText(chunks))

	// An ordinary unspaced word larger than a whole chunk is hard-split on
	// rune boundaries (lossless); only protected atoms (URLs/tokens) degrade.
	giant := strings.Repeat("Z", 5000)
	hardSplit, err := msgpkg.Split(giant, 4000)
	require.NoError(t, err)
	require.Len(t, hardSplit, 2)
	for _, chunk := range hardSplit {
		require.True(t, utf8.ValidString(chunk.Text))
	}
	require.Equal(t, giant, joinText(hardSplit))

	// A protected atom (URL) that cannot fit is degraded with an omitted
	// count rather than cut into an unclickable half-link.
	giantURL := "https://example.internal/" + strings.Repeat("segment/", 600)
	require.Greater(t, utf8.RuneCountInString(giantURL), 4000)
	degraded, err := msgpkg.Split(giantURL, 4000)
	require.NoError(t, err)
	require.NotEmpty(t, degraded)
	require.Contains(t, degraded[0].Text, "omitted")
	require.True(t, utf8.ValidString(degraded[0].Text))
}

func joinText(chunks []msgpkg.Chunk) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString(c.Text)
	}
	return b.String()
}
