// Package redact removes credentials from values the SDK writes to its logs.
package redact

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/textproto"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
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

// URL returns raw with any embedded credential replaced, so an endpoint can be
// logged without leaking what it carries.
//
// A URL of the form scheme://user:password@host is a working authentication
// mechanism: Go's http.Client turns userinfo into a Basic Authorization header,
// and Pyroscope ingest accepts it. The SDK therefore has to keep honouring such
// endpoints while never writing one verbatim to stdout or the OTLP log
// pipeline, both of which carry the record out of the process. A presigned
// endpoint carries its credential in the query instead, so credentialQueryKeys
// is handled on the same terms.
//
// The contract is deliberately one-sided: a value is echoed only when every
// "@" in it has been positively accounted for as something other than
// userinfo. Anything else — an endpoint that does not parse, or one where a
// credential could be hiding in a position url.Parse does not treat as
// userinfo — is replaced wholesale, because the cost of a less useful log line
// is far below the cost of printing a secret.
//
// The redaction itself is URLAttribute's, applied to the parsed value rather
// than restated here: a span attribute and a log line are the same problem, and
// the last time the two carried separate copies of this logic one of them was
// missing the opaque-URL check.
func URL(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return redactedWhole
	}
	if u.User == nil && u.Opaque == "" && !hasCredentialQuery(raw) {
		// Nothing here needs replacing, so the operator's exact string is
		// returned rather than url.URL.String()'s normalisation of it.
		return raw
	}
	return URLAttribute(u)
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
// A presigned URL carries its credential in the query rather than the userinfo,
// so it has no "@" for that rule to hold on to. It gets a rule of its own on
// the same terms: a credential query parameter is accepted only where this
// package wrote its value, and anything else takes the message with it. See
// unaccountedCredentialQuery.
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
	// The rule is checked by counting, not by rewriting and looking again.
	//
	// Each match of the pattern above ends at an "@" and cannot contain one, so
	// the number of matches is exactly the number of at-signs this function can
	// account for. Any other "@" is one it cannot, and the text goes wholesale.
	//
	// Two earlier forms of this check were talked out of the rule by their own
	// output. Stripping "redacted@" from the finished text stripped the input's
	// own as readily, so a message holding "//alice:redacted@host" — which the
	// pattern does not match, being scheme-relative — lost the "@" that made it
	// fail. Substituting a fixed sentinel instead moved the collision rather
	// than removing it: text already holding that sentinel had it swapped for a
	// "redacted@" the check never accounted for. Counting compares the text
	// against itself and introduces nothing, so there is no collision to have.
	if strings.Count(text, "@") != len(urlUserinfo.FindAllStringIndex(text, -1)) {
		return redactedWhole
	}
	// The second closed rule, for the credential a presigned URL carries in its
	// query instead of its userinfo. Such a URL has no "@" at all, so the rule
	// above passes it through untouched.
	//
	// Both rules are anchored on characters, and a character can reach this
	// text encoded. The one that matters is "&": encoding/json escapes it as
	// \u0026, so a signed URL quoted inside a Go server's JSON error body
	// reads "?tenant=t\u0026Signature=..." and the separator the query rule
	// holds on to is not there. Rather than teach the pattern that spelling —
	// the enumeration this package refuses elsewhere — the rule is applied a
	// second time to the text with its \u escapes decoded, so any encoding of
	// a separator resolves to the character the rule already knows.
	decoded := jsonUnescaped(text)
	if unaccountedCredentialQuery(text) || (decoded != text && unaccountedCredentialQuery(decoded)) {
		return redactedWhole
	}
	return urlUserinfo.ReplaceAllString(text, "${1}"+placeholder+"@")
}

// jsonUnescaped returns text with its \uXXXX escapes replaced by the characters
// they stand for, leaving everything else exactly as it is.
//
// It is not a JSON decoder and does not try to be one: the text it is handed is
// a log line that may merely contain a JSON fragment, so it cannot be unquoted,
// and the only thing the closed rules need is for an encoded separator to
// resolve to its character. A malformed escape is left alone, which keeps the
// function total — it can only ever fail to decode, and failing to decode
// leaves the rules looking at the same text they looked at before.
func jsonUnescaped(text string) string {
	if !strings.Contains(text, `\u`) {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); {
		if i+6 <= len(text) && text[i] == '\\' && text[i+1] == 'u' {
			if r, err := strconv.ParseUint(text[i+2:i+6], 16, 32); err == nil {
				b.WriteRune(rune(r))
				i += 6
				continue
			}
		}
		b.WriteByte(text[i])
		i++
	}
	return b.String()
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

// queryParam matches the beginning of any query parameter anywhere in a
// string: a "?" or "&", the key, and the "=" that opens its value.
//
// It deliberately does not spell out the credential keys. A key is
// percent-encoded text, and net/url decodes it before anything sees it
// (url.parseQuery runs QueryUnescape on the key, not only the value), so
// "?Sign%61ture=" is the parameter "Signature" to every server and to
// url.Values. A pattern matching literal spellings would let that through
// while redactQuery, which decodes, treats the same URL as a credential —
// the two disagreeing is how an encoded key reached a log line. So the
// pattern finds the keys and isCredentialQueryKey decides, once, for both.
var queryParam = regexp.MustCompile(`[?&]([^&=?#\s"']*)=`)

// isCredentialQueryKey reports whether a raw query key, as it appears in a
// URL, names one of credentialQueryKeys once decoded.
//
// A key that cannot be unescaped is compared as it stands: it decodes to
// nothing a server would read as a known key, and guessing at it would only
// risk mangling a parameter that is not a credential.
func isCredentialQueryKey(rawKey string) bool {
	decoded, err := url.QueryUnescape(rawKey)
	if err != nil {
		decoded = rawKey
	}
	_, ok := credentialQueryKeys[strings.ToLower(decoded)]
	return ok
}

// hasCredentialQuery reports whether text carries a credential query
// parameter at all, whatever its value.
func hasCredentialQuery(text string) bool {
	for _, match := range queryParam.FindAllStringSubmatchIndex(text, -1) {
		if isCredentialQueryKey(text[match[2]:match[3]]) {
			return true
		}
	}
	return false
}

// escapedOpaquePlaceholder is what redactQuery writes in place of a credential
// query value, and therefore the only value unaccountedCredentialQuery accepts.
var escapedOpaquePlaceholder = url.QueryEscape(opaquePlaceholder)

// unaccountedCredentialQuery reports whether text holds a credential query
// parameter this package did not redact itself.
//
// It is InText's closed rule for presigned URLs, and it is a separate rule
// because the "@" one cannot speak for them: a signature travels in the query,
// so a URL carrying one has no "@" anywhere and passes the userinfo check
// untouched. That is how a signed profiling endpoint reached a log line — the
// uploader appends "/ingest" and re-encodes the query before formatting the URL
// into its message, so the configured endpoint is no longer a substring for
// InText to substitute, and nothing else looked at the query.
//
// Each value is read to the first character that cannot appear unescaped inside
// one — "&", whitespace, or a quote — and compared against the placeholder
// redactQuery writes. Reading short is safe: it can only make a value differ
// from the placeholder, which fails closed. Nothing can be forged past it
// either, because the only accepted value is the placeholder in full: text
// arriving with "Signature=%5Bredacted%5D" already carries "[redacted]" as its
// signature, and a longer value with that prefix does not match.
func unaccountedCredentialQuery(text string) bool {
	for _, match := range queryParam.FindAllStringSubmatchIndex(text, -1) {
		if !isCredentialQueryKey(text[match[2]:match[3]]) {
			continue
		}
		value := text[match[1]:]
		if end := strings.IndexAny(value, "&\"' \t\r\n"); end >= 0 {
			value = value[:end]
		}
		if value != escapedOpaquePlaceholder {
			return true
		}
	}
	return false
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
// The closed rule URL uses applies here too: a value is echoed only when every
// "@" in it has been accounted for as something other than userinfo. An opaque
// URL — "http:alice:secret@host", which url.Parse leaves in Opaque with User
// nil — would otherwise render right back out with the credential in it.
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
	if opaqueMayHoldCredentials(redacted.Opaque) || strings.Contains(redacted.Host, "@") {
		return redactedWhole
	}
	return redacted.String()
}

// opaqueMayHoldCredentials reports whether an opaque URL's payload could be
// carrying a credential.
//
// url.Parse only recognises userinfo in a hierarchical URL. Given an opaque one
// it attributes nothing: "http:alice:secret@host" leaves the whole of
// "alice:secret@host" in Opaque with User nil, so a caller that only replaced
// User would render the credential straight back out.
//
// Two shapes are refused. An "@" is the one userinfo cannot do without. A ":"
// is the separator of the "user:pass" pair itself, which is what is left when
// an operator writes such a URL without a host — nothing sends it, since
// url.URL.User is nil, but it is still a secret they typed.
//
// A payload with neither is a single token and cannot be a pair. That is what
// keeps a scheme-less "pyroscope:4040" legible: url.Parse reads the host as the
// scheme and leaves "4040" here. Such an endpoint does not actually work in this
// SDK — url.URL.String() ignores Path when Opaque is set, so the exporters' path
// defaults are silently dropped — which is exactly when an operator most needs
// to see what they configured.
func opaqueMayHoldCredentials(opaque string) bool {
	return strings.ContainsAny(opaque, ":@")
}

// redactQuery replaces the values of credentialQueryKeys in a raw query
// string, reporting whether anything changed.
//
// The query is rewritten in place rather than through url.Values, whose Encode
// sorts the parameters and re-escapes every value: url.full is meant to be the
// URL that was requested, and reordering it would make a span attribute that no
// longer matches the access log beside it. Which keys count is
// isCredentialQueryKey's decision, shared with the rule InText applies to free
// text, so a percent-encoded key cannot be a credential to one and not the
// other.
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
		if !hasValue || !isCredentialQueryKey(key) {
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

// GoEscaped returns v as strconv.Quote renders it between the quotes, the form
// a value takes wherever something prints it with %q.
//
// A secret has to be listed in both forms: a caller that reports a value
// verbatim and one that quotes it produce different text for the same
// credential, and Secrets matches literally. It lives in this package rather
// than beside one of its callers because "which renderings of a secret must be
// matched" is the same question wherever a secret list is built.
func GoEscaped(v string) string {
	q := strconv.Quote(v)
	return q[1 : len(q)-1]
}

// JSONEscaped returns v as encoding/json renders it between the quotes, the
// form a value takes in a JSON document written by a Go program.
//
// It is not GoEscaped with different punctuation. encoding/json escapes "<",
// ">" and "&" as "\u003c", "\u003e" and "\u0026" by default — SetEscapeHTML
// is on unless a caller turns it off — and renders a control byte as "\u0000"
// where %q renders "\x00". A Pyroscope or OpenTelemetry Collector deployment
// is a Go program, and both put an error into a JSON body, so a server echoing
// back the header it received produces exactly this rendering of it.
//
// Marshalling a string cannot fail; v is returned unchanged if it somehow does.
func JSONEscaped(v string) string {
	encoded, err := json.Marshal(v)
	if err != nil || len(encoded) < 2 {
		return v
	}
	return string(encoded[1 : len(encoded)-1])
}

// Renderings returns the forms a secret can take in text the SDK may be asked
// to scrub: as it stands, as %q renders it, and as a JSON document holds it.
//
// Secrets matches literally, so every rendering something might print has to be
// listed separately. This is the one place that decides which — the two secret
// lists in this SDK each used to expand a value themselves, and every round
// that added a rendering to one and not the other left the pair disagreeing
// about what counts as the same credential.
//
// Duplicates are dropped, so a value the escaping leaves alone is returned
// once. Empty strings are not filtered here; the caller's own list does that.
func Renderings(v string) []string {
	forms := make([]string, 0, 3)
	for _, form := range []string{v, GoEscaped(v), JSONEscaped(v)} {
		if !slices.Contains(forms, form) {
			forms = append(forms, form)
		}
	}
	return forms
}

// HeaderWireValue returns v as net/http writes it into a request, which is the
// form a server sees and may echo back.
//
// A configured header value is not necessarily sent verbatim: Header.Set stores
// it untouched, but the write path trims leading and trailing ASCII whitespace
// (net/http/header.go writeSubset, via textproto.TrimString). So an
// Authorization header configured as " Bearer token " goes out as
// "Bearer token", and a response or an error that quotes what it received holds
// a string neither the configured value nor its Go-escaped form matches.
//
// Trimming is the whole transformation. writeSubset also rewrites CR and LF as
// spaces, but a request header holding either never reaches it: Transport
// validates every value with httpguts.ValidHeaderFieldValue first and fails the
// request with "invalid header field value for %q", which names the header
// rather than what it held.
//
// It lives beside GoEscaped for the same reason: which renderings of a secret
// must be matched is the same question wherever a secret list is built, and the
// two lists in this SDK should not answer it differently.
func HeaderWireValue(v string) string {
	return strings.Trim(v, " \t\r\n")
}

// HeaderWireName returns a header name as net/http stores and sends it.
//
// Header.Set does not key by the name it was given: it keys by
// textproto.CanonicalMIMEHeaderKey(name), so "x-api-key" goes out as
// "X-Api-Key". A name that holds a byte which cannot appear in a header name
// is stored and sent unchanged.
//
// It matters for the same reason names are in a secret list at all — a
// credential pasted into the wrong side of a header configuration is still a
// credential, and a server or an exporter that reports the header it received
// names the canonical form, not the configured one.
func HeaderWireName(name string) string {
	return textproto.CanonicalMIMEHeaderKey(name)
}

// HeaderWireNameHTTP2 returns the form HTTP/2 puts a header name on the wire
// in: lowercase, per RFC 7540 8.1.2.
//
// net/http does not send the canonical form over an h2 connection. It encodes
// each name through httpcommon.LowerHeader (net/http/internal/httpcommon), so
// the same request that sends "X-Api-Key" over HTTP/1.1 sends "x-api-key" over
// HTTP/2 — and a TLS endpoint negotiates h2 by default. A name that is not
// printable ASCII is returned unchanged, matching LowerHeader's own fallback.
func HeaderWireNameHTTP2(name string) string {
	for i := 0; i < len(name); i++ {
		if c := name[i]; c < 0x20 || c > 0x7e {
			return name
		}
	}
	return strings.ToLower(name)
}

// CookieWireValue returns the form an HTTP/2 peer receives a Cookie header in.
//
// A Cookie value is the one header net/http does not send verbatim over h2. The
// client splits it on ";" and emits a separate field per pair, stripping the
// spaces that followed each separator (net/http/internal/httpcommon, the
// "cookie" branch of the request encoder); the server then rejoins the fields
// with "; ". So a value written as "session=x;tenant=y" is read back as
// "session=x; tenant=y", which neither the configured form nor HeaderWireValue
// of it matches.
//
// A value with no ";" is returned unchanged, so this costs nothing for every
// other header.
func CookieWireValue(value string) string {
	if !strings.Contains(value, ";") {
		return value
	}
	fields := strings.Split(value, ";")
	for i, field := range fields {
		fields[i] = strings.TrimLeft(field, " ")
	}
	return strings.Join(fields, "; ")
}

// BasicAuthHeader returns the Authorization value net/http derives from a URL's
// userinfo, or "" when the URL carries none.
//
// An endpoint of the form scheme://user:pass@host is a working authentication
// mechanism precisely because http.Client turns it into a header: send() sets
// "Authorization: Basic " + base64(user + ":" + pass) whenever req.URL.User is
// set and no Authorization header was given. That base64 is the credential in a
// form no other entry in a secret list holds — it contains neither the username
// nor the password as substrings — so a server echoing the header it received
// would defeat every other rendering the list carries.
func BasicAuthHeader(rawURL string) string {
	if rawURL == "" || !strings.Contains(rawURL, "@") {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.User == nil {
		return ""
	}
	password, _ := u.User.Password()
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+password))
}

// HeaderValueForms returns every rendering of a configured header value that
// something between this SDK and the server may produce: as configured, as
// net/http trims it, as an HTTP/2 peer rejoins it when it is a Cookie, and each
// of those as %q and as a JSON document render them.
//
// It exists so the SDK's two secret lists cannot answer that question
// differently. They each used to expand a value themselves, and every round of
// review that added a rendering to one and not the other left a credential
// matched in one pipeline and not the other.
func HeaderValueForms(value string) []string {
	wire := HeaderWireValue(value)
	var forms []string
	for _, base := range []string{value, wire, CookieWireValue(wire)} {
		forms = append(forms, Renderings(base)...)
	}
	return forms
}

// HeaderNameForms returns the renderings of a configured header name: as
// configured, as Header.Set keys it, and as HTTP/2 sends it.
//
// Names are listed because a credential pasted into the wrong side of a header
// configuration is still a credential, and the name is what an error or an
// echoing server reports.
func HeaderNameForms(name string) []string {
	var forms []string
	for _, base := range []string{name, HeaderWireName(name), HeaderWireNameHTTP2(name)} {
		forms = append(forms, Renderings(base)...)
	}
	return forms
}

// Error returns err with credentials removed from its message, keeping the
// error chain intact.
//
// An error travels further than the attribute beside it: a wrapped one is
// rendered into a log line by whatever handles it, and into a span's
// exception.message by RecordError. Redacting the attribute while returning
// the credential in the error moves the leak rather than removing it.
//
// endpoints and secrets are the values the caller already knows are sensitive,
// as InText and Secrets take them; either may be nil.
//
// err is returned unchanged when nothing needed redacting, so a caller
// comparing error values — not only matching with errors.Is — is unaffected in
// the ordinary case. Otherwise the result unwraps to err, so errors.Is and
// errors.As still reach whatever the caller is matching on; only the rendered
// message changes.
func Error(err error, endpoints, secrets []string) error {
	if err == nil {
		return nil
	}
	original, rendered := renderError(err)
	msg := Secrets(errorText(original, err, endpoints), secrets...)
	if rendered && msg == original {
		return err
	}
	// A broken error is wrapped even when its placeholder needs no redaction.
	// Returning it unchanged would hand the caller back the value whose Error
	// method panics, and every caller here renders the result again.
	return &redactedError{msg: msg, err: err}
}

// errorText renders err's message with its credentials removed.
//
// A *url.Error is handled through its own URL field rather than by matching the
// rendered message. net/http builds that field with the password masked and the
// username kept, and never touches the query, so it is exactly the value a
// url.full attribute would have carried — and substituting a redaction of the
// parsed URL is precise where pattern-matching the message is guesswork.
// Anything else falls back to InText's closed rule.
//
// Each URL is substituted in both the form it holds and the form %q renders it
// as: url.Error.Error formats the field with %q, so a URL containing a quote or
// a control character appears in the message only escaped, and replacing the
// raw form alone would be a silent no-op.
//
// A URL the parser cannot account for takes the whole message with it. The
// message quotes such a URL in full, and neither URLAttribute nor InText's "@"
// rule can speak for what is inside it — a signed query carries its credential
// with no "@" anywhere. The span keeps url.full, server.address and error.type,
// so the request is still identifiable without it.
func errorText(text string, err error, endpoints []string) string {
	urls := errorURLs(err)
	// Longest first. A nested *url.Error commonly holds the outer one's URL
	// plus a query, so replacing the outer first would rewrite its prefix
	// inside the inner occurrence too — and the inner URL, no longer matching,
	// would keep whatever its query carried.
	sort.Slice(urls, func(i, j int) bool { return len(urls[i]) > len(urls[j]) })
	for _, raw := range urls {
		parsed, parseErr := url.Parse(raw)
		if parseErr != nil {
			return redactedWhole
		}
		redacted := URLAttribute(parsed)
		text = strings.ReplaceAll(text, raw, redacted)
		if escaped := GoEscaped(raw); escaped != raw {
			text = strings.ReplaceAll(text, escaped, redacted)
		}
	}
	return InText(text, endpoints...)
}

// errorURLs returns the URL of every *url.Error in err's chain.
//
// errors.As would stop at the first: net/http wraps a transport error in an
// outer *url.Error, so a RoundTripper that performs a request of its own
// leaves a second one nested inside, with its own URL quoted into the same
// message. Both have to be accounted for.
//
// Nothing is called on a nil pointer along the way. errors.As reports a typed
// nil as a match, and both (*url.Error).Unwrap and its Error method read a
// field, so a chain holding one would otherwise panic here — in Shutdown, that
// would skip every closer still to run.
//
// The walk is bounded by the number of errors it visits in total, not by how
// deep it goes. A depth cap alone is no bound at all once Unwrap returns a
// slice: an error whose Unwrap() []error holds itself twice branches in two at
// every step, so a cap of n permits on the order of 2^n visits. Shutdown calls
// this synchronously, ahead of the closers still to run, so an error from a
// dependency must not be able to hold it there.
func errorURLs(err error) []string {
	var urls []string
	budget := maxErrorChainVisits
	var walk func(error)
	walk = func(e error) {
		if e == nil || budget <= 0 {
			return
		}
		budget--
		if rv := reflect.ValueOf(e); rv.Kind() == reflect.Pointer && rv.IsNil() {
			return
		}
		// The assertion is on this link only, deliberately: errors.As would
		// jump to the first match in the whole chain and report a typed nil as
		// one, which is the pair of behaviours this walk exists to avoid.
		if urlErr, ok := e.(*url.Error); ok && urlErr.URL != "" { //nolint:errorlint // walking the chain link by link is the point
			urls = append(urls, urlErr.URL)
		}
		// Likewise: this switches on the unwrap interfaces to take the next
		// step, which is what errors.Is and errors.As do internally, not on a
		// concrete error type.
		switch unwrapper := e.(type) { //nolint:errorlint // dispatching on Unwrap, not on an error type
		case interface{ Unwrap() error }:
			walk(unwrapped(unwrapper.Unwrap))
		case interface{ Unwrap() []error }:
			for _, sub := range unwrappedAll(unwrapper.Unwrap) {
				walk(sub)
			}
		}
	}
	walk(err)
	return urls
}

// maxErrorChainVisits bounds the total number of errors the walk over a chain
// the SDK did not build will visit. It is generous next to any chain a real
// error carries, and finite next to one that unwraps into itself.
const maxErrorChainVisits = 1024

// redactedError renders a redacted message while still unwrapping to the error
// it was built from.
type redactedError struct {
	msg string
	err error
}

// Error returns the redacted message, which is what every ordinary rendering
// of the error — fmt, slog.Any, errors.Join, RecordError — ends up printing.
func (e *redactedError) Error() string { return e.msg }

// Unwrap returns the error this was built from, so errors.Is and errors.As
// reach past the redaction to whatever the caller is matching on.
func (e *redactedError) Unwrap() error { return e.err }

// ErrorText renders err for a diagnostic record without letting a broken error
// value take the process down: a typed nil pointer is named rather than
// dereferenced by its own Error method, and an Error method that panics is
// recovered into a placeholder naming the type. A diagnostic is never worth a
// crash.
//
// It lives beside the redaction because every path that renders an arbitrary
// error for the SDK's own output goes through both, and an error the SDK did
// not construct is exactly the one that can be broken: Error, the logr sink and
// the SDK's ErrorHandler all render through it.
func ErrorText(err error) string {
	text, _ := renderError(err)
	return text
}

// renderError renders err, reporting whether the error rendered itself or the
// guard had to stand in for it. Error needs that apart from the text: a
// placeholder can equal the redacted message while the error behind it is still
// one whose Error method panics, and handing that back would move the crash to
// the caller.
func renderError(err error) (text string, rendered bool) {
	if rv := reflect.ValueOf(err); rv.Kind() == reflect.Pointer && rv.IsNil() {
		return fmt.Sprintf("<nil %T>", err), false
	}
	defer func() {
		if r := recover(); r != nil {
			text, rendered = fmt.Sprintf("[omitted: %T panicked while rendering]", err), false
		}
	}()
	return err.Error(), true
}

// unwrapped calls an Unwrap method the SDK did not write, and reports no next
// link if it panics.
//
// renderError guards the Error method for the same reason, and a dependency's
// Unwrap is a second way into the same crash: from Shutdown a panic here would
// skip every closer still to run. Only the broken branch of the walk stops —
// the links already collected stand.
func unwrapped(unwrap func() error) (next error) {
	defer func() {
		if r := recover(); r != nil {
			next = nil
		}
	}()
	return unwrap()
}

// unwrappedAll is unwrapped for the multi-error form.
func unwrappedAll(unwrap func() []error) (next []error) {
	defer func() {
		if r := recover(); r != nil {
			next = nil
		}
	}()
	return unwrap()
}
