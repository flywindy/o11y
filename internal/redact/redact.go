// Package redact removes credentials from values the SDK writes to its logs.
package redact

import (
	"net/url"
	"regexp"
	"strings"
)

// placeholder replaces userinfo that was present but must not be logged. It
// keeps the fact that credentials were configured visible — useful when an
// operator is diagnosing an auth failure — without revealing them.
const placeholder = "redacted"

// redactedWhole replaces an endpoint whose structure could not be accounted
// for. Echoing such a value risks printing a credential the parser did not
// attribute to userinfo.
const redactedWhole = "[endpoint redacted]"

// URL returns raw with any embedded userinfo replaced, so an endpoint can be
// logged without leaking the credentials it carries.
//
// A URL of the form scheme://user:password@host is a working authentication
// mechanism: Go's http.Client turns userinfo into a Basic Authorization header,
// and Pyroscope ingest accepts it. The SDK therefore has to keep honouring such
// endpoints while never writing one verbatim to stdout or the OTLP log
// pipeline, both of which carry the record out of the process.
//
// The contract is deliberately one-sided: a value is echoed only when every
// "@" in it has been positively accounted for as something other than
// userinfo. Anything else — an endpoint that does not parse, or one where a
// credential could be hiding in a position url.Parse does not treat as
// userinfo — is replaced wholesale, because the cost of a less useful log line
// is far below the cost of printing a secret.
func URL(raw string) string {
	if raw == "" || !strings.Contains(raw, "@") {
		// No userinfo is possible without an "@", so the common case skips
		// parsing entirely and returns the operator's exact string.
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return redactedWhole
	}
	if u.User != nil {
		u.User = url.User(placeholder)
		return u.String()
	}
	// No userinfo was parsed, yet the value contains "@". url.Parse only
	// recognises userinfo in a hierarchical URL ("scheme://user:pass@host");
	// given an opaque one ("scheme:user:pass@host") it leaves the credential
	// in Opaque, where returning raw would print it in full.
	if strings.Contains(u.Opaque, "@") || strings.Contains(u.Host, "@") {
		return redactedWhole
	}
	// Every remaining "@" sits in the path, query, or fragment — positions the
	// parser has attributed, and which userinfo cannot occupy.
	return raw
}

// urlUserinfo matches the userinfo of a hierarchical URL anywhere in a string:
// a scheme, "://", then everything up to the first "@". Userinfo cannot contain
// an unencoded "/" or "@", so stopping at the first one keeps the match inside
// a single URL.
var urlUserinfo = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/@\s"']+@`)

// InText returns text with credentials removed, for text that is about to be
// logged. The userinfo of every URL-shaped substring is replaced, and any
// occurrence of one of knownEndpoints is replaced by its redacted form.
//
// Redacting an endpoint attribute alone is not enough: net/url renders a parse
// failure as `parse "<raw>": …`, so an error logged beside a redacted endpoint
// hands back the very credential the redaction removed. Pyroscope's client
// parses the address and returns that error verbatim, which puts the leak on
// the same warning that reports the failure.
//
// The scrub is deliberately not keyed on the configured endpoint alone. The
// value that ends up in the error is not always the one the SDK configured —
// pyroscope-go overrides it from PYROSCOPE_ADHOC_SERVER_ADDRESS before parsing
// (api.go:57-59 in v1.3.0) — so matching only known endpoints would pass an
// overridden one straight through. knownEndpoints still matter because they
// catch opaque forms ("scheme:user:pass@host") that the URL shape above does
// not.
func InText(text string, knownEndpoints ...string) string {
	for _, endpoint := range knownEndpoints {
		if endpoint == "" || !strings.Contains(text, endpoint) {
			continue
		}
		text = strings.ReplaceAll(text, endpoint, URL(endpoint))
	}
	return urlUserinfo.ReplaceAllString(text, "${1}"+placeholder+"@")
}
