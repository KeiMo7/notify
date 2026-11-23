package message

import (
	"net"
	"regexp"
	"strings"
)

// RedactedMarker replaces every sensitive fragment. It is a fixed ASCII
// token so redaction is identical across every channel and every log sink,
// and cleartext secrets never leave the process in plain form.
const RedactedMarker = "[REDACTED]"

var (
	// credentialURL matches URLs with embedded userinfo, e.g.
	// https://admin:hunter2@db.internal:5432/secret — both the credentials
	// and the internal host are sensitive in alert traffic.
	credentialURL = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.\-]*://[^\s/@]+@[^\s/]+(?:/[^\s]*)?`)

	// internalHostname matches hosts under internal/special-use name
	// suffixes (RFC 6762/2606) plus bare localhost, with or without a port
	// or URL scheme. These denote infrastructure that must not leak.
	internalHostname = regexp.MustCompile(
		`(?i)\b(?:[a-z][a-z0-9+.\-]*://)?` +
			`(?:[a-z0-9](?:[a-z0-9\-]{0,61}[a-z0-9])?\.)+` +
			`(?:internal|local|localhost|lan|corp|home|arpa|intranet)\b` +
			`(?::\d+)?(?:/[^\s]*)?`)

	// bareLocalhost matches the literal "localhost" host, optionally with a port.
	bareLocalhost = regexp.MustCompile(`(?i)\blocalhost(?::\d+)?\b`)

	// secretAssignment matches key=value style secrets such as
	// token=abc..., api_key: abc..., authorization "abc...". Quoted values
	// may contain spaces; unquoted values run to the next whitespace or
	// field separator.
	secretAssignment = regexp.MustCompile(
		`(?i)\b(token|api[_-]?key|apikey|secret|password|passwd|authorization|auth|credential|access[_-]?key|private[_-]?key)` +
			`(\s*[:=]\s*)` +
			`(?:"([^"]*)"|'([^']*)'|([^\s"';,)]+))`)

	// bearerToken matches "Bearer <jwt-or-token>" headers.
	bearerToken = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._\-]+`)

	// jwt matches standalone JWT-looking tokens.
	jwtToken = regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\b`)
)

// Redact applies the single, channel-independent secrecy policy. It must run
// before any logging, formatting, escaping or transport step so that no code
// path can emit a raw secret. Redaction is purely substitutive: it never
// reinterprets the surrounding text.
func Redact(msg string) string {
	msg = credentialURL.ReplaceAllStringFunc(msg, func(match string) string {
		// Preserve the scheme so receivers still understand the fragment,
		// but drop userinfo and the (internal) host entirely.
		scheme := match[:strings.Index(match, "://")+3]
		return scheme + RedactedMarker
	})
	msg = internalHostname.ReplaceAllString(msg, RedactedMarker)
	msg = bareLocalhost.ReplaceAllString(msg, RedactedMarker)
	msg = bearerToken.ReplaceAllString(msg, "Bearer "+RedactedMarker)
	msg = jwtToken.ReplaceAllString(msg, RedactedMarker)
	msg = secretAssignment.ReplaceAllStringFunc(msg, func(match string) string {
		sub := secretAssignment.FindStringSubmatch(match)
		key, sep := sub[1], sub[2]
		switch {
		case sub[3] != "":
			// double-quoted value: keep the quote characters
			return key + sep + "\"" + RedactedMarker + "\""
		case sub[4] != "":
			// single-quoted value
			return key + sep + "'" + RedactedMarker + "'"
		default:
			return key + sep + RedactedMarker
		}
	})
	msg = redactPrivateIPs(msg)
	return msg
}

// redactPrivateIPs masks loopback, link-local and RFC1918 addresses, with or
// without a port. Public addresses are left untouched.
func redactPrivateIPs(msg string) string {
	var out strings.Builder
	out.Grow(len(msg))
	for i := 0; i < len(msg); {
		// Find the next candidate start: a digit (IPv4) or hex-colon (IPv6).
		c := msg[i]
		if !isIPCandidateByte(c) {
			out.WriteByte(c)
			i++
			continue
		}
		// Try progressively longer candidates up to the longest
		// host(:port) form we reasonably expect.
		end := scanIPCandidateEnd(msg, i)
		consumed := false
		for end > i {
			host, port := splitHostPort(msg[i:end])
			if ip := net.ParseIP(host); ip != nil && isInternalIP(ip) {
				out.WriteString(RedactedMarker)
				if port != "" {
					out.WriteString(":" + port)
				}
				i = end
				consumed = true
				break
			}
			// Shrink the candidate at non-identifier boundaries.
			end = prevCandidateEnd(msg, i, end)
		}
		if !consumed {
			out.WriteByte(c)
			i++
		}
	}
	return out.String()
}

func isIPCandidateByte(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') || c == ':'
}

// scanIPCandidateEnd returns the index just past a maximal run of characters
// that could belong to an IP literal or an IP:port pair.
func scanIPCandidateEnd(msg string, start int) int {
	end := start
	for end < len(msg) {
		c := msg[end]
		switch {
		case c >= '0' && c <= '9',
			c >= 'a' && c <= 'f',
			c >= 'A' && c <= 'F',
			c == ':', c == '.':
			end++
		default:
			return end
		}
	}
	return end
}

// prevCandidateEnd shrinks [start,end) by walking back over the trailing
// identifier run, stopping at the last '.' or ':' separator.
func prevCandidateEnd(msg string, start, end int) int {
	e := end - 1
	for e > start && msg[e-1] != '.' && msg[e-1] != ':' {
		e--
	}
	return e
}

// splitHostPort splits a trailing :port when the host side parses as an IP.
// IPv6 literals without brackets are handled by trying the raw string first.
func splitHostPort(candidate string) (host, port string) {
	if ip := net.ParseIP(candidate); ip != nil {
		return candidate, ""
	}
	lastColon := strings.LastIndex(candidate, ":")
	if lastColon > 0 {
		h, p := candidate[:lastColon], candidate[lastColon+1:]
		if net.ParseIP(h) != nil && isAllDigits(p) {
			return h, p
		}
	}
	return candidate, ""
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isInternalIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified()
}
