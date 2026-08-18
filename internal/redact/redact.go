// Package redact removes credentials from values the SDK writes to its logs.
package redact

import (
	"net/url"
	"strings"
)

// placeholder replaces userinfo that was present but must not be logged. It
// keeps the fact that credentials were configured visible — useful when an
// operator is diagnosing an auth failure — without revealing them.
const placeholder = "redacted"

// URL returns raw with any embedded userinfo replaced, so an endpoint can be
// logged without leaking the credentials it carries.
//
// A URL of the form scheme://user:password@host is a working authentication
// mechanism: Go's http.Client turns userinfo into a Basic Authorization header,
// and Pyroscope ingest accepts it. The SDK therefore has to keep honouring such
// endpoints while never writing one verbatim to stdout or the OTLP log
// pipeline, both of which carry the record out of the process.
//
// A value that does not parse is returned unchanged when it cannot contain
// userinfo, and reduced to a fixed placeholder when it might: an endpoint the
// SDK cannot interpret is not one it can prove is safe to log.
func URL(raw string) string {
	if raw == "" || !strings.Contains(raw, "@") {
		// No userinfo is possible without an "@", so the common case skips
		// parsing entirely and returns the operator's exact string.
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "[unparseable endpoint redacted]"
	}
	if u.User == nil {
		return raw
	}
	u.User = url.User(placeholder)
	return u.String()
}
