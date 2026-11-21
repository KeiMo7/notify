<h1 align="center">
  <img src="static/notify-logo.png" alt="notify" width="200px">
  <br>
</h1>


<p align="center">
<a href="https://opensource.org/licenses/MIT"><img src="https://img.shields.io/badge/license-MIT-_red.svg"></a>
<a href="https://github.com/projectdiscovery/notify/issues"><img src="https://img.shields.io/badge/contributions-welcome-brightgreen.svg?style=flat"></a>
<a href="https://goreportcard.com/badge/github.com/projectdiscovery/notify"><img src="https://goreportcard.com/badge/github.com/projectdiscovery/notify"></a>
<a href="https://github.com/projectdiscovery/notify/releases"><img src="https://img.shields.io/github/release/projectdiscovery/notify"></a>
<a href="https://hub.docker.com/r/projectdiscovery/notify"><img src="https://img.shields.io/docker/pulls/projectdiscovery/notify.svg"></a>
<a href="https://twitter.com/pdiscoveryio"><img src="https://img.shields.io/twitter/follow/pdiscoveryio.svg?logo=twitter"></a>
<a href="https://discord.gg/projectdiscovery"><img src="https://img.shields.io/discord/695645237418131507.svg?logo=discord"></a>
</p>

<p align="center">
  <a href="#features">Features</a> •
  <a href="#notify-installation">Installation</a> •
  <a href="#provider-config">Providers</a> •
  <a href="#usage">Usage</a> •
  <a href="#running-notify">Running Notify</a> •
  <a href="#notes">Notes</a> •
  <a href="https://discord.gg/projectdiscovery">Join Discord</a>
</p>


Notify is a Go-based assistance package that enables you to stream the output of several tools (or read from a file) and publish it to a variety of supported platforms.

<h1 align="left">
  <img src="static/notify-httpx.png" alt="notify-httpx" width="700px">
  <br>
</h1>

# Features

- Supports for Slack / Discord / Telegram
- Supports for Pushover / Email
- Supports for Microsoft Teams / Google Chat
- Supports for File / Pipe input
- Supports Line by Line / Bulk Post
- Supports using Single / Multiple providers
- Supports Custom Web-hooks
- Supports Custom data formatting 


# Usage

```sh
notify -h
```

This will display help for the tool. Here are all the switches it supports.

| Flag                    | Description                                        | Example                               |
|-------------------------|----------------------------------------------------|---------------------------------------|
| `-bulk`                 | enable bulk processing                             | `notify -bulk`                        |
| `-char-limit`           | max character limit per message (default 4000)     | `notify -cl 2000`                     |
| `-config`               | notify configuration file                          | `notify -config config.yaml`          |
| `-data`                 | input file to send for notify                      | `notify -i test.txt`                  |
| `-delay`                | delay in seconds between each notification         | `notify -d 2`                         |
| `-id`                   | id to send the notification to (optional)          | `notify -id recon,scans`              |
| `-msg-format`           | add custom formatting to message                   | `notify -mf Hey {{data}}`             |
| `-no-color`             | disable colors in output                           | `notify -nc`                          |
| `-provider-config`      | provider config path                               | `notify -pc provider.yaml`            |
| `-provider`             | provider to send the notification to (optional)    | `notify -p slack,telegram`            |
| `-proxy`                | HTTP/SOCKSv5 proxy to use with notify              | `notify -proxy http://127.0.0.1:8080` |
| `-rate-limit`           | maximum number of HTTP requests to send per second | `notify -rl 1`                        |
| `-silent`               | enable silent mode                                 | `notify -silent`                      |
| `-verbose`              | enable verbose mode                                | `notify -verbose`                     |
| `-version`              | display version                                    | `notify -version`                     |
| `-update`               | updates to latest version                          | `notify -update`                      |
| `-disable-update-check` | disables automatic update check                    | `notify -duc`                         |
| `-reliability`          | enable failure handling (retries, circuit breakers, dedup, spill queue) | `notify -reliability` |

# Failure handling (reliability mode)

Notify can optionally protect the push pipeline against channel failures
without changing any existing channel adapter or message format. The whole
feature is **opt-in**: it stays dormant unless enabled, and when disabled the
send path is byte-for-byte the legacy one.

Enable it either with the `-reliability` flag or by adding a
`reliability` section to the provider config. The flag switches the feature
on with all parameters defaulted; the YAML section is the only place to tune
individual parameters.

```yaml
reliability:
  enabled: true
  max_retries: 3                 # retries per send (the original attempt is not counted)
  backoff_initial: 500ms         # first retry delay, then exponential: initial*2^n
  backoff_max: 10s               # cap on a single retry delay
  max_retry_time: 15s            # wall-clock cap for retries of one send
  failure_threshold: 5           # consecutive failures before a channel is paused
  cooldown: 60s                  # how long a paused channel stays skipped before a probe
  dedup_window: 0s               # >0 merges identical messages inside one window
  queue_max_entries: 1000        # spill-queue bound; overflow drops the OLDEST and counts
  queue_path: ""                 # default: next to the provider config
```

## Failure classes and what happens to each

Classification uses the channel's **explicit signals only** — never response
time. HTTP status codes and the `Retry-After` header are read through an
instrumented transport; non-HTTP channels (e.g. SMTP) fall back to the
protocol error text, and a rate-limit verdict still requires an explicit
token (`429`, `Retry-After`, "rate limit"/"too many requests").

| Class | Signals | Disposition |
|---|---|---|
| Rate limited | HTTP 429, or 503 with `Retry-After` | bounded exponential backoff retry; `Retry-After` is honored when larger |
| Authentication failed | 401 / 403 / 407, SMTP 535, invalid token | **no retry**, immediate error-level alert |
| Content rejected | 400 / 413 / 422 / 415 | no retry (the identical payload would fail again) |
| Unreachable | connection reset/refused, DNS failure, timeout, 5xx, 404/410 | retried with backoff |
| Unknown | anything unattributable | retried conservatively |

Retries are bounded two ways — `max_retries` (attempt count) and
`max_retry_time` (wall clock) — so a single send performs at most
`1 + max_retries` requests and total push time never exceeds the legacy
upper bound plus one bounded retry budget.

## Channel degradation (circuit breaker)

When a channel fails `failure_threshold` times in a row it is **paused**:
subsequent sends skip it with zero network requests, an alert is emitted, and
every other channel continues normally. After `cooldown` exactly one probe
is allowed through; success restores the channel, failure re-pauses it.
Degraded channels and failure counts are observable in the status logs
emitted on every state transition.

## Duplicate suppression

With `dedup_window` greater than zero, identical messages arriving inside one
window are merged: the first is delivered immediately, repeats are counted,
and a single summary (`N duplicate notifications merged …`) is pushed when
the window closes. Windows close lazily on the next message — there is no
background timer goroutine.

## Local spill queue and replay

If **every** configured channel fails (or is paused), the message is written
to a local on-disk queue instead of being dropped. On the next startup the
queue is replayed automatically in FIFO order; entries are acknowledged only
after delivery, so a crash cannot lose or duplicate them. The queue is
bounded by `queue_max_entries`: overflow evicts the oldest entries and keeps
a persistent `dropped` counter visible in the status logs. A corrupt queue
file is quarantined (`<path>.corrupt`) rather than taking the process down.

## Known limitations

- Rate-limit detection for channels whose client uses a private HTTP
  transport (Gotify with TLS verification disabled) relies on the adapter's
  error text instead of status headers; all other channels use the transport
  signals.
- The spill queue is a single local file, best suited to one notify process;
  do not run multiple processes against the same `queue_path`.
- Backoff sleeps are part of the bounded per-send budget; with very small
  `max_retry_time` values fewer retries will fit and the budget, not the
  attempt count, becomes the effective limit.

# Notify Installation

```sh
go install -v github.com/projectdiscovery/notify/cmd/notify@latest
```

### Provider Config

The default provider config file can be created at `$HOME/.config/notify/provider-config.yaml` and can have the following contents:

```yaml
slack:
  - id: "slack"
    slack_channel: "recon"
    slack_username: "test"
    slack_format: "{{data}}"
    slack_icon_emoji: ":ghost:"
    slack_webhook_url: "https://hooks.slack.com/services/XXXXXX"

  - id: "vulns"
    slack_channel: "vulns"
    slack_username: "test"
    slack_format: "{{data}}"
    slack_icon_emoji: ":ghost:"
    slack_webhook_url: "https://hooks.slack.com/services/XXXXXX"

discord:
  - id: "crawl"
    discord_channel: "crawl"
    discord_username: "test"
    discord_format: "{{data}}"
    discord_webhook_url: "https://discord.com/api/webhooks/XXXXXXXX"

  - id: "subs"
    discord_channel: "subs"
    discord_username: "test"
    discord_format: "{{data}}"
    discord_webhook_url: "https://discord.com/api/webhooks/XXXXXXXX"

telegram:
  - id: "tel"
    telegram_api_key: "XXXXXXXXXXXX"
    telegram_chat_id: "XXXXXXXX" # Optional topic id XXXXXXXX:Y
    telegram_thread_id: "YYYY" # optional forum topic / thread ID
    telegram_format: "{{data}}"
    telegram_parsemode: "Markdown" # None/Markdown/MarkdownV2/HTML (https://core.telegram.org/bots/api#formatting-options)
    telegram_preview: false # set true to enable link previews

pushover:
  - id: "push"
    pushover_user_key: "XXXX"
    pushover_api_token: "YYYY"
    pushover_format: "{{data}}"
    pushover_devices:
      - "iphone"

smtp:
  - id: email
    smtp_server: mail.example.com
    smtp_username: test@example.com
    smtp_password: password
    from_address: from@email.com
    smtp_cc:
      - to@email.com
    smtp_format: "{{data}}"
    subject: "Email subject"
    smtp_html: false
    smtp_disable_starttls: false

googlechat:
  - id: "gc"
    key: "XXXXXXXX"
    token: "XXXXXX"
    space: "XXXXXX"
    google_chat_format: "{{data}}"

teams:
  - id: "recon"
    teams_webhook_url: "https://<domain>.webhook.office.com/webhookb2/xx@xx/IncomingWebhook/xx"
    teams_format: "{{data}}"

gotify:
  - id: 'gotify'
    gotify_host: 'XXXXXX'
    gotify_port: '80'
    gotify_token: 'XXXXXX'
    gotify_format: '{{data}}'
    gotify_disabletls: false
    gotify_title: "recon"

custom:
  - id: webhook
    custom_webhook_url: http://host/api/webhook
    custom_method: GET
    custom_format: '{{data}}'
    custom_headers:
      Content-Type: application/json
      X-Api-Key: XXXXX
      
custom:
  - id: webhookJson
    custom_webhook_url: http://host/api/webhook
    custom_method: GET
    custom_format: '{"text":{{dataJsonString}} }'
    custom_headers:
      Content-Type: application/json
      X-Api-Key: XXXXX

custom:
  - id: webhook
    custom_webhook_url: http://host/api/webhook
    custom_method: GET
    custom_sprig: '{"text":"{{ .url }}"}'
    custom_headers:
      Content-Type: application/json
      X-Api-Key: XXXXX
``` 

# Running Notify

Notify supports piping output of any tool or output file and send it to configured provider/s (e.g, discord, slack channel) as notification.

### Send notification using piped(stdin) output

```sh
subfinder -d hackerone.com | notify -bulk
```

<h1 align="left">
<img width="365" alt="notify-subfinder" src="https://user-images.githubusercontent.com/8293321/130240854-e3031bc6-ecc8-47f8-9654-4c58e09cc622.png">
</h1>

### Send notification using output file


```sh
subfinder -d hackerone.com -o h1.txt; notify -data h1.txt
```

### Send notification using output file in bulk mode


```sh
subfinder -d hackerone.com -o h1.txt; notify -data h1.txt -bulk
```

### Send notification using output file to specific provider's


```sh
subfinder -d hackerone.com -o h1.txt; notify -data h1.txt -bulk -provider discord,slack
```

### Send notification using output file to specific ID's


```sh
subfinder -d hackerone.com -o h1.txt; notify -data h1.txt -bulk -id recon,vulns,scan
```

### Example Uses

Following command will enumerate subdomains using [SubFinder](https://github.com/projectdiscovery/subfinder) and probe alive URLs using [httpx](https://github.com/projectdiscovery/httpx), runs [Nuclei](https://github.com/projectdiscovery/nuclei) templates and send the nuclei results as a notifications to configured provider/s.


```sh
subfinder -d intigriti.com | httpx | nuclei -tags exposure -o output.txt; notify -bulk -data output.txt
```


### Provider Config


The tool tries to use the default provider config (`$HOME/.config/notify/provider-config.yaml`), it can also be specified via CLI by using **provider-config** flag.

To run the tool with custom provider config, just use the following command.

```sh
notify -provider-config providers.yaml
```

### Notify Config

Notify flags can be configured at default config (`$HOME/.config/notify/config.yaml`) or custom config can be also provided using `config` flag.

## Notes
- As default notify sends notification line by line
- Use `-bulk` to send notification as entire message/s (messages might be chunked)
- Reliability mode is opt-in (`-reliability` or a `reliability:` config section); with it off, behavior and request volume are identical to older versions
- The spill queue file lives next to the provider config (`provider-config.yaml.pending.json` by default) and is safe to inspect while notify is not running; do not hand-edit it

## References

- [Creating Slack webhook](https://slack.com/intl/en-it/help/articles/115005265063-Incoming-webhooks-for-Slack)
- [Creating Discord webhook](https://support.discord.com/hc/en-us/articles/228383668-Intro-to-Webhooks)
- [Creating Telegram bot](https://core.telegram.org/bots#3-how-do-i-create-a-bot)
- [Creating Pushover Token](https://github.com/containrrr/shoutrrr/blob/main/docs/services/pushover.md)

Notify is made with 🖤 by the [projectdiscovery](https://projectdiscovery.io) team.
