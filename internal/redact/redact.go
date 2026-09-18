// Package redact removes credentials from values the SDK writes to its logs.
package redact

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
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
// a single URL. It is a best-effort tidier, not the safety property — see
// InText.
var urlUserinfo = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/@\s"']+@`)

// InText returns text with credentials removed, for text that is about to be
// logged.
//
// Redacting an endpoint attribute alone is not enough: net/url renders a parse
// failure as `parse "<raw>": …`, so an error logged beside a redacted endpoint
// hands back the very credential the redaction removed. Pyroscope's client
// parses the address and returns that error verbatim, which puts the leak on
// the same warning that reports the failure.
//
// The safety property here is deliberately *not* pattern matching. Earlier
// versions of this function tried to enumerate the shapes a credential can
// take, and each round of review found another one they missed: an endpoint
// overridden via PYROSCOPE_ADHOC_SERVER_ADDRESS so it matches no known value;
// an opaque URL ("scheme:user:pass@host") or a scheme-relative one
// ("//user:pass@host"), neither carrying the "://" the pattern anchors on; a
// quote or space inside the userinfo, which net/url escapes so the raw endpoint
// no longer appears in the error at all. Chasing shapes is unbounded, so the
// rule is closed instead:
//
//	userinfo cannot exist without an "@", so if no "@" survives, no
//	credential survives.
//
// The known endpoints and the pattern above run first because they keep the
// common cases readable. Whatever they leave behind is checked against that
// rule, and text that fails it is replaced wholesale rather than reasoned
// about. Losing the detail of one warning costs an operator an indirection; a
// leaked credential costs a rotation.
func InText(text string, knownEndpoints ...string) string {
	for _, endpoint := range knownEndpoints {
		if endpoint == "" || !strings.Contains(text, endpoint) {
			continue
		}
		text = strings.ReplaceAll(text, endpoint, URL(endpoint))
	}
	text = urlUserinfo.ReplaceAllString(text, "${1}"+placeholder+"@")
	// The check runs against a probe with this function's own placeholders
	// stripped: a successful substitution leaves a "redacted@" behind, and
	// counting that as an unaccounted-for "@" would discard every text it just
	// made safe.
	if strings.Contains(strings.ReplaceAll(text, placeholder+"@", ""), "@") {
		return redactedWhole
	}
	return text
}

// opaquePlaceholder replaces a secret Secrets was told about.
const opaquePlaceholder = "[redacted]"

// shortSecretLen is the length below which a secret is replaced only where
// it stands as a whole token. A configured header value such as "1" or
// "true" is a secret the caller must not print, but replacing every "1"
// inside timestamps and counts would mangle the rest of the line; matching
// it only between non-alphanumeric characters (the "1" in "value=1", not
// the one in "attempt 12" or "13T11:00") keeps both.
const shortSecretLen = 6

// Secrets returns text with every occurrence of each non-empty secret
// replaced by a placeholder. It is for values that carry no structure the
// other rules can recognise — an OTLP header value such as a bearer token,
// which the pinned exporters echo verbatim when the OTEL_EXPORTER_OTLP_HEADERS
// value fails to parse — so the caller names them up front. Longer secrets
// are replaced first, so a whole "k=v,k2=v2" string and its parts can both
// be listed without the parts breaking the whole. A secret shorter than
// shortSecretLen is replaced only where it is a whole token (not adjacent
// to a letter or digit), so a short configured value is still covered
// where an exporter echoes it on its own without rewriting every number
// in the message.
func Secrets(text string, secrets ...string) string {
	if len(secrets) == 0 || text == "" {
		return text
	}
	ordered := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		if secret != "" {
			ordered = append(ordered, secret)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	for _, secret := range ordered {
		if len(secret) < shortSecretLen {
			text = replaceWholeToken(text, secret)
			continue
		}
		text = strings.ReplaceAll(text, secret, opaquePlaceholder)
	}
	return text
}

// replaceWholeToken replaces each occurrence of secret in text that is not
// adjacent to a letter or digit on either side.
func replaceWholeToken(text, secret string) string {
	var b strings.Builder
	pos := 0
	for pos < len(text) {
		i := strings.Index(text[pos:], secret)
		if i < 0 {
			break
		}
		start := pos + i
		end := start + len(secret)
		if tokenBoundaryBefore(text, start) && tokenBoundaryAfter(text, end) {
			b.WriteString(text[pos:start])
			b.WriteString(opaquePlaceholder)
		} else {
			b.WriteString(text[pos:end])
		}
		pos = end
	}
	b.WriteString(text[pos:])
	return b.String()
}

func tokenBoundaryBefore(text string, i int) bool {
	if i == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(text[:i])
	return !isTokenRune(r)
}

func tokenBoundaryAfter(text string, end int) bool {
	if end == len(text) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(text[end:])
	return !isTokenRune(r)
}

func isTokenRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// credentialQueryKeys are the query parameters OpenTelemetry semantic
// conventions v1.39.0 names as credentials that a url.full attribute must not
// carry: "AWSAccessKeyId", "Signature", "sig", "X-Goog-Signature". They are
// spelled out here because they appear in the specification's prose for the
// attribute rather than as constants in the generated semconv package, which
// only defines attribute keys.
//
// Comparison is case-insensitive. The spec names one key that differs from the
// others only in case convention, and a signed URL is generated by a cloud SDK
// rather than typed, so matching exactly what the spec prints would leave a
// presigned URL from a client that capitalises differently unredacted.
var credentialQueryKeys = map[string]struct{}{
	"awsaccesskeyid":   {},
	"signature":        {},
	"sig":              {},
	"x-goog-signature": {},
}

// URLAttribute returns u rendered for a url.full span attribute: userinfo
// replaced, and the values of the credential-bearing query parameters above
// replaced, with the rest of the URL left intact so the span still identifies
// the request that was made.
//
// A span attribute leaves the process exactly as a log record does, so the
// same rule applies: an outbound URL may carry userinfo, which Go turns into a
// Basic Authorization header, and a presigned URL carries its signature in the
// query. semconv v1.39.0 says url.full SHOULD have both removed.
//
// u is not modified. The caller's URL belongs to the request, which a client
// may reuse across retries, so the copy is made here rather than left to every
// call site to remember.
func URLAttribute(u *url.URL) string {
	if u == nil {
		return ""
	}
	redacted := *u
	if redacted.User != nil {
		redacted.User = url.User(placeholder)
	}
	if query := redacted.RawQuery; query != "" {
		if scrubbed, changed := redactQuery(query); changed {
			redacted.RawQuery = scrubbed
		}
	}
	return redacted.String()
}

// redactQuery replaces the values of credentialQueryKeys in a raw query
// string, reporting whether anything changed.
//
// The query is rewritten in place rather than through url.Values, whose Encode
// sorts the parameters and re-escapes every value: url.full is meant to be the
// URL that was requested, and reordering it would make a span attribute that no
// longer matches the access log beside it. A parameter whose key cannot be
// unescaped is left as it stands — it matches none of the known keys, and
// guessing at it would only risk mangling a value that is not a credential.
func redactQuery(rawQuery string) (string, bool) {
	var (
		builder strings.Builder
		changed bool
	)
	builder.Grow(len(rawQuery))
	for i, pair := range strings.Split(rawQuery, "&") {
		if i > 0 {
			builder.WriteByte('&')
		}
		key, _, hasValue := strings.Cut(pair, "=")
		decoded, err := url.QueryUnescape(key)
		if err != nil {
			decoded = key
		}
		if _, ok := credentialQueryKeys[strings.ToLower(decoded)]; !ok || !hasValue {
			builder.WriteString(pair)
			continue
		}
		builder.WriteString(key)
		builder.WriteByte('=')
		builder.WriteString(url.QueryEscape(opaquePlaceholder))
		changed = true
	}
	return builder.String(), changed
}
