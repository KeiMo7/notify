package reliability

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// signalRecord is one HTTP round-trip observed by the instrumented
// transport. A single provider send may perform several round-trips
// (redirects), so an attempt aggregates the records collected between
// BeginAttempt and EndAttempt.
type signalRecord struct {
	statusCode   int
	retryAfter   time.Duration // 0 when the channel did not advertise one
	transportErr bool
}

// Classify turns the HTTP signals observed during an attempt, together with
// the error the provider adapter returned, into a FailureClass and an
// optional channel-advertised retry delay.
//
// Precedence: explicit channel signals always win over error-text fallback.
// Rate limiting is therefore never guessed from latency: it requires an
// HTTP 429 or a Retry-After header on a 503.
func Classify(records []signalRecord, attemptErr error) (FailureClass, time.Duration) {
	var retryAfter time.Duration

	if len(records) > 0 {
		class := ClassUnknown
		for _, rec := range records {
			if rec.retryAfter > retryAfter {
				retryAfter = rec.retryAfter
			}
			recClass := classifyRecord(rec)
			// Successful sub-requests (redirects followed inside one attempt)
			// must not mask a failure observed on another round-trip.
			if recClass == ClassOK {
				continue
			}
			class = mergeClass(class, recClass)
		}
		// A 429/503+Retry-After must surface as rate limited regardless of
		// the other records in the same attempt.
		for _, rec := range records {
			if rec.statusCode == http.StatusTooManyRequests ||
				(rec.statusCode == http.StatusServiceUnavailable && rec.retryAfter > 0) {
				return ClassRateLimited, retryAfter
			}
		}
		return class, retryAfter
	}

	return classifyFromError(attemptErr), 0
}

// mergeClass combines two observations from the same attempt. Deterministic
// failures (auth, content) outrank transient ones because retrying them is
// pointless; rate limiting outranks the generic transient buckets.
func mergeClass(a, b FailureClass) FailureClass {
	rank := func(c FailureClass) int {
		switch c {
		case ClassRateLimited:
			return 5
		case ClassAuthFailed:
			return 4
		case ClassContentRejected:
			return 3
		case ClassUnreachable:
			return 2
		case ClassUnknown:
			return 1
		default:
			return 0
		}
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

func classifyRecord(rec signalRecord) FailureClass {
	if rec.transportErr {
		// Connection reset, DNS failure, timeout: the target was not reachable.
		return ClassUnreachable
	}
	switch {
	case rec.statusCode == http.StatusTooManyRequests:
		return ClassRateLimited
	case rec.statusCode == http.StatusUnauthorized,
		rec.statusCode == http.StatusForbidden,
		rec.statusCode == http.StatusProxyAuthRequired:
		return ClassAuthFailed
	case rec.statusCode == http.StatusBadRequest,
		rec.statusCode == http.StatusRequestEntityTooLarge,
		rec.statusCode == http.StatusUnprocessableEntity,
		rec.statusCode == http.StatusUnsupportedMediaType:
		return ClassContentRejected
	case rec.statusCode >= 500:
		// 503 without Retry-After is a generic transient outage, not a
		// rate-limit signal: the channel told us nothing about quota.
		return ClassUnreachable
	case rec.statusCode == http.StatusNotFound,
		rec.statusCode == http.StatusGone:
		return ClassUnreachable
	case rec.statusCode > 0 && rec.statusCode < 400:
		// Successful/redirect responses are not failures on their own.
		return ClassOK
	default:
		return ClassUnknown
	}
}

// classifyFromError is the fallback for paths that do not surface an HTTP
// response to the instrumented transport (SMTP, shoutrrr URL parse errors,
// clients with a private Transport such as Gotify-with-TLS-disabled). It
// parses the adapter error text, which all providers embed the protocol
// answer into. It must never invent a rate-limit verdict without an explicit
// token from the channel (429 / "too many requests" / "rate limit").
func classifyFromError(err error) FailureClass {
	if err == nil {
		return ClassOK
	}
	text := strings.ToLower(err.Error())

	switch {
	case strings.Contains(text, "429"),
		strings.Contains(text, "too many requests"),
		strings.Contains(text, "rate limit"),
		strings.Contains(text, "rate-limit"),
		strings.Contains(text, "throttl"):
		return ClassRateLimited
	case strings.Contains(text, "401"),
		strings.Contains(text, "403"),
		strings.Contains(text, "unauthorized"),
		strings.Contains(text, "forbidden"),
		strings.Contains(text, "invalid token"),
		strings.Contains(text, "invalid api key"),
		strings.Contains(text, "authentication required"),
		strings.Contains(text, "auth failed"),
		// SMTP permanent authentication rejection per RFC 5321.
		strings.Contains(text, "535"):
		return ClassAuthFailed
	case strings.Contains(text, "400"),
		strings.Contains(text, "413"),
		strings.Contains(text, "422"),
		strings.Contains(text, "payload too large"),
		strings.Contains(text, "message too long"),
		strings.Contains(text, "invalid format"),
		strings.Contains(text, "bad request"):
		return ClassContentRejected
	case strings.Contains(text, "500"),
		strings.Contains(text, "502"),
		strings.Contains(text, "503"),
		strings.Contains(text, "504"),
		strings.Contains(text, "connection reset"),
		strings.Contains(text, "connection refused"),
		strings.Contains(text, "broken pipe"),
		strings.Contains(text, "no such host"),
		strings.Contains(text, "dial tcp"),
		strings.Contains(text, "i/o timeout"),
		strings.Contains(text, "deadline exceeded"),
		strings.Contains(text, "eof"),
		strings.Contains(text, "temporarily unavailable"):
		return ClassUnreachable
	default:
		return ClassUnknown
	}
}

// parseRetryAfter interprets the Retry-After header, which is either a number
// of seconds or an HTTP date. A malformed header yields 0 so a channel cannot
// make us sleep on garbage; malformed upstream data never crashes the path.
func parseRetryAfter(header string) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	if secs, err := strconv.Atoi(header); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(header); err == nil {
		d := time.Until(when)
		if d > 0 {
			return d
		}
		return 0
	}
	return 0
}
