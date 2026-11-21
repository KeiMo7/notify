# Reliability: degradation and replay record sample

This is a worked record of the failure-handling feature, showing the exact
log/queue artifacts produced. It is illustrative (timestamps shortened); the
field names match the code in `pkg/reliability`.

## 1. Rate limiting with bounded backoff

A channel answers HTTP `429` with `Retry-After: 2`. The send retries within
the configured budget (`max_retries: 3`, exponential backoff starting at
500ms, capped by `max_retry_time: 15s`). Total requests for this one message:
**4** (1 original + 3 retries) — the retries never exceed the cap.

```
[WRN] reliability: rate limited by provider "discord" after 4 attempt(s), backing off: failed to send discord notification for id: crawl
[INF] reliability: provider discord retry 1/3 after 2s (channel Retry-After honored)
[INF] reliability: provider discord retry 2/3 after 4s
[INF] reliability: provider discord retry 3/3 after 8s
```

If every retry is still rejected, the failure is counted toward the breaker
and the message continues to the other channels.

## 2. Authentication failure — no retry, immediate alert

```
[ERR] reliability: AUTH FAILURE on provider "telegram", not retrying: failed to send telegram notification for id: tel: 401 Unauthorized
```

Exactly one request was made. The breaker sees the failure, but the alert is
error-level because credentials need an operator.

## 3. Channel degradation (circuit open)

After `failure_threshold: 5` consecutive failures the channel pauses. The
state-transition alert is followed by the full status snapshot; later sends
skip the channel with **zero** requests (`skipped_while_open` increments).

```
[WRN] reliability: provider "discord" DEGRADED — circuit open for 1m0s, other providers continue
[WRN] reliability: provider=discord state=open consecutive_failures=5 total_failures=5 retries=9 rate_limited=5 auth_failed=0 content_rejected=0 unreachable=0 skipped_while_open=0
[WRN] reliability: provider=slack    state=closed consecutive_failures=0 total_failures=0 retries=0 rate_limited=0 auth_failed=0 content_rejected=0 unreachable=0 skipped_while_open=0
[WRN] reliability: spill queue path=/home/ops/.config/notify/provider-config.yaml.pending.json length=0 dropped_oldest=0
```

After `cooldown` the next send is admitted as a single half-open probe; a
200 closes the breaker (`state=closed`, streak reset to 0), another failure
re-opens it and restarts the cooldown.

## 4. All channels unavailable — spill to the local queue

Every configured channel failed (or was open), so the message is persisted
rather than dropped:

```
[WRN] reliability: all providers unavailable, message stored in local queue (/home/ops/.config/notify/provider-config.yaml.pending.json, pending=1)
```

On-disk queue file (pretty-printed JSON, atomic temp-file + rename):

```json
{
  "dropped": 0,
  "entries": [
    {
      "enqueued_at": "2026-11-22T02:13:44.812903Z",
      "message": "https://example.com/acme/.git/config — git config exposure"
    }
  ]
}
```

When the backlog exceeds `queue_max_entries`, the **oldest** entries are
evicted and counted, e.g. after two overflows:

```json
{
  "dropped": 2,
  "entries": [ "…newest 1000 entries…" ]
}
```

```
[WRN] reliability: spill queue full (cap=1000), oldest entries dropped; total dropped=2
```

## 5. Replay after recovery

On the next startup the queue is replayed in FIFO order; each entry is
acknowledged only after at least one channel accepts it, so a crash mid-replay
leaves the remaining entries on disk for another attempt.

```
[INF] reliability: replayed queued message from 2026-11-22T02:13:44.812903Z
[INF] reliability: replayed 1 queued message(s), 0 still pending
```

If a channel is still down, replay stops at the first undeliverable entry and
keeps the rest pending:

```
[WRN] reliability: replay stops at queue position 0, 1 entries remain pending
```

## 6. Duplicate suppression

With `dedup_window: 5m`, the same result arriving repeatedly is delivered once
and merged thereafter:

```
[INF] reliability: duplicate merged (+1 suppressed within window 5m0s)
[INF] reliability: duplicate merged (+2 suppressed within window 5m0s)
```

When the window closes, one summary push replaces all suppressed copies:

```
[notify] 3 duplicate notifications merged within the dedup window:
https://example.com/acme/.git/config — git config exposure
```
