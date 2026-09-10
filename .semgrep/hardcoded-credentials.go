// Package testdata holds the fixtures for hardcoded-credentials.yml.
//
// `semgrep scan --test` reads the annotations: a `ruleid:` comment names the
// rule that must fire on the line directly below it. A line with no
// annotation is a negative assertion — a rule firing there is reported as a
// false positive. Both directions carry weight: the positives pin the
// declaration forms the rule covers, and the negatives pin the shapes it
// deliberately does not, so a pattern edit that narrows or widens the rule
// fails here instead of quietly changing what a later scan reports.
//
// Coverage is per independently editable branch: each syntactic form and each
// alternative of the name regex gets its own line, so deleting any one of
// them fails here rather than silently narrowing the gate.
//
// The file sits beside hardcoded-credentials.yml because semgrep's test
// runner matches a rule file to a target of the same basename and does not
// support a separate tests directory.
//
// Every planted credential below carries `// nosemgrep: gosec.G101-1` in
// place. GOSEC_FLAGS, SEMGREP_FLAGS and .semgrepignore all exclude this
// directory, but every one of those is repo-local: a scan that reads files
// directly rather than walking the tree honors none of them and reports this
// file whatever the ignore list holds. That is not hypothetical — it is what
// happened the first time this branch went through an external scan.
//
// The directive is a trailing comment rather than its own line because
// `ruleid:` binds to the line directly below it, so a directive above the
// code would take the slot the test runner needs. It names `gosec.G101-1`
// rather than being a bare `nosemgrep` so `hardcoded-credential-literal`
// stays live on every line and this file's own assertions still fail if the
// rule regresses — nosemgrep matches a rule id by exact suffix, so naming one
// never silences the other.
package testdata

// --- positives: one line per declaration form ---

// ruleid: hardcoded-credential-literal
const constFormPassword = "sup3rs3cr3t" // nosemgrep: gosec.G101-1

// ruleid: hardcoded-credential-literal
var varFormSecret = "sup3rs3cr3t" // nosemgrep: gosec.G101-1

// An explicit type between the name and "=" is still a declaration, not a
// different pattern shape — Go credential declarations commonly spell out the
// type (`const apiToken string = "..."`), and a rule that only matched the
// untyped form would miss most of them.
//
// ruleid: hardcoded-credential-literal
const typedConstFormToken string = "sup3rs3cr3t" // nosemgrep: gosec.G101-1

// ruleid: hardcoded-credential-literal
var typedVarFormApiKey string = "sup3rs3cr3t" // nosemgrep: gosec.G101-1

// declarationForms covers the two patterns that only appear inside a function
// body. The package-level const and var above complete the set of four.
func declarationForms() {
	// ruleid: hardcoded-credential-literal
	shortDeclPassword := "sup3rs3cr3t" // nosemgrep: gosec.G101-1

	var assignedSecret string
	// ruleid: hardcoded-credential-literal
	assignedSecret = "sup3rs3cr3t" // nosemgrep: gosec.G101-1

	_, _ = shortDeclPassword, assignedSecret
}

// multiValueForms covers Go's multi-value short/plain assignment
// (`a, b := x, y`), a shape distinct from the single-value patterns above —
// it's the one place credentials commonly appear paired with an unrelated
// value (`user, password := "admin", "..."`).
func multiValueForms(lookup func(string) string) {
	// ruleid: hardcoded-credential-literal
	user, password := "admin", "sup3rs3cr3t" // nosemgrep: gosec.G101-1

	// ruleid: hardcoded-credential-literal
	var apiUser, apiSecret = "admin", "sup3rs3cr3t" // nosemgrep: gosec.G101-1

	// The credential-named half of the pair is negative here: password comes
	// from lookup(), not a literal, so only requestID's assignment is a
	// string literal and it carries no credential word. No ruleid — a match
	// on this line means the pair is no longer matched positionally.
	password, requestID := lookup("APP_PASSWORD"), "abc-request-id" // nosemgrep: gosec.G101-1

	_, _, _, _, _, _ = user, password, apiUser, apiSecret, password, requestID
}

// --- positives: one line per alternative of the name regex ---

// nameAlternatives gives each alternative of the rule's name regex its own
// line, so dropping one from the pattern fails here instead of narrowing the
// gate.
func nameAlternatives() {
	// ruleid: hardcoded-credential-literal
	password := "value-that-is-long-enough" // nosemgrep: gosec.G101-1
	// ruleid: hardcoded-credential-literal
	passwd := "value-that-is-long-enough" // nosemgrep: gosec.G101-1
	// ruleid: hardcoded-credential-literal
	pwd := "value-that-is-long-enough" // nosemgrep: gosec.G101-1
	// ruleid: hardcoded-credential-literal
	secret := "value-that-is-long-enough" // nosemgrep: gosec.G101-1
	// ruleid: hardcoded-credential-literal
	token := "value-that-is-long-enough" // nosemgrep: gosec.G101-1
	// ruleid: hardcoded-credential-literal
	apiKey := "value-that-is-long-enough" // nosemgrep: gosec.G101-1
	// ruleid: hardcoded-credential-literal
	credential := "value-that-is-long-enough" // nosemgrep: gosec.G101-1

	_, _, _, _, _, _, _ = password, passwd, pwd, secret, token, apiKey, credential
}

// separatedNameAlternatives covers the three terms whose halves carry no
// meaning apart — "api key", "access key" and "private key". Split spellings
// are what code ported from JSON, env files or another language brings in,
// and a contiguous-only match walks straight past them.
func separatedNameAlternatives() {
	// ruleid: hardcoded-credential-literal
	api_key := "live-secret-value" // nosemgrep: gosec.G101-1
	// ruleid: hardcoded-credential-literal
	accessKey := "AKIAIOSFODNN7EXAMPLE" // nosemgrep: gosec.G101-1
	// ruleid: hardcoded-credential-literal
	access_key := "AKIAIOSFODNN7EXAMPLE" // nosemgrep: gosec.G101-1
	// A pasted PEM block is the highest-impact shape in this whole file, and
	// "key" alone is excluded, so the qualified spellings have to be listed
	// or the gate never sees it.
	// ruleid: hardcoded-credential-literal
	privateKey := "-----BEGIN PRIVATE KEY-----MIIEvQIBADAN" // nosemgrep: gosec.G101-1
	// ruleid: hardcoded-credential-literal
	private_key := "-----BEGIN PRIVATE KEY-----MIIEvQIBADAN" // nosemgrep: gosec.G101-1

	_, _, _, _, _ = api_key, accessKey, access_key, privateKey, private_key
}

// A bare "key" is not a credential word. This library names attribute,
// metric and object keys constantly, and every one of them holds a literal —
// widening the qualifier to plain "key" would flag the lot.
func aBareKeyIsNotACredential() {
	objectKey := "videos/2026/clip.bin"
	traceIDKey := "traceId"

	_, _ = objectKey, traceIDKey
}

// The match is on a substring of the identifier, not the whole of it, so a
// qualified name still trips the rule — the shape internal/redact's own test
// fixtures actually take (`const secret = "..."`).
func qualifiedNames() {
	// ruleid: hardcoded-credential-literal
	proxyPassword := "value-that-is-long-enough" // nosemgrep: gosec.G101-1
	// ruleid: hardcoded-credential-literal
	clientSecretValue := "value-that-is-long-enough" // nosemgrep: gosec.G101-1
	// usernamePassword embeds "name" mid-identifier. It is here because an
	// earlier version of the rule exempted identifiers by name and had to be
	// careful not to veto this one; the exemptions are gone, but the case is
	// worth keeping — a credential word anywhere in the identifier counts.
	// ruleid: hardcoded-credential-literal
	usernamePassword := "value-that-is-long-enough" // nosemgrep: gosec.G101-1

	_, _, _ = proxyPassword, clientSecretValue, usernamePassword
}

// valueForms pins the two literal shapes the value regex accepts. The escaped
// case is the one a naive regex misses: an embedded quote must not end the
// match, or `"p\"ass"` reads as a non-literal and walks straight past the
// gate.
func valueForms() {
	// ruleid: hardcoded-credential-literal
	escapedPassword := "p\"ass" // nosemgrep: gosec.G101-1
	// ruleid: hardcoded-credential-literal
	rawSecret := `raw-string-secret` // nosemgrep: gosec.G101-1

	_, _ = escapedPassword, rawSecret
}

// concatenatedValueForms covers a credential split across "+" — every piece
// is still a literal, so the whole expression is exactly as hard-coded as
// one string, just shaped to dodge a naive single-literal check.
func concatenatedValueForms() {
	// ruleid: hardcoded-credential-literal
	concatenatedSecret := "sup3r-" + "secret" // nosemgrep: gosec.G101-1

	_ = concatenatedSecret
}

// concatenationWithADynamicPartIsNotCovered is the negative twin: as soon as
// one piece is not a literal, the expression is no longer "assembled
// entirely from literals" and must stay silent — the value could be
// anything, including something read from the environment.
func concatenationWithADynamicPartIsNotCovered(suffix string) string {
	return "sup3r-" + suffix
}

// --- negatives: shapes the rule must not flag ---

// A struct-literal field is deliberately out of scope: it is not a
// declaration, so none of the four patterns above reaches one, and widening
// to cover it would trade one real finding for every table-driven test that
// builds a Config.
type config struct {
	ProxyPassword string
	ClientSecret  string
}

// structLiteralFieldIsNotCovered is the negative assertion for the composite
// literal: no ruleid annotation, so the test runner reports a match here as a
// false positive if a pattern ever widens into that form.
func structLiteralFieldIsNotCovered() config {
	return config{ProxyPassword: "proxypass", ClientSecret: "s"} // nosemgrep: gosec.G101-1
}

// --- positives: protocol constants are flagged too, and annotated ---

// An identifier that names a header, cookie, collection or env var holds a
// protocol constant rather than a credential, but the rule flags it anyway.
// It has to: `headerAuthToken = "x-auth-token"` and
// `headerPassword = "production-secret"` are indistinguishable by name, and
// their values share every lexical shape, so any silent exemption wide enough
// to clear the first also clears the second.
//
// Real code would settle one of these with
// `// nosemgrep: gosec.G101-1, hardcoded-credential-literal` plus a reason,
// putting the judgement in the diff where a reviewer sees it. Here the
// directive names gosec only: `ruleid:` asserts that our own rule fires, so
// suppressing it would assert the opposite.
const (
	// ruleid: hardcoded-credential-literal
	headerAuthToken = "x-auth-token" // nosemgrep: gosec.G101-1
	// ruleid: hardcoded-credential-literal
	ssoTokenName = "ssoToken" // nosemgrep: gosec.G101-1
	// ruleid: hardcoded-credential-literal
	ssoTokenHeader = "ssoToken" // nosemgrep: gosec.G101-1
	// ruleid: hardcoded-credential-literal
	ssoTokensCollection = "sso_tokens" // nosemgrep: gosec.G101-1
	// ruleid: hardcoded-credential-literal
	passwordFieldName = "password" // nosemgrep: gosec.G101-1
	// ruleid: hardcoded-credential-literal
	tokenEnvVar = "SSO_TOKEN" // nosemgrep: gosec.G101-1
)

// The shapes the earlier name-based exemption let through silently: a real
// credential under an identifier that merely looks like it names something.
// Each must fire.
func credentialUnderAQualifierName() {
	// ruleid: hardcoded-credential-literal
	apiKeyHeader := "sk-live-abc123realkey" // nosemgrep: gosec.G101-1
	// ruleid: hardcoded-credential-literal
	headerPassword := "production-secret" // nosemgrep: gosec.G101-1
	// ruleid: hardcoded-credential-literal
	passwordEnvVar := "hunter2-real-password" // nosemgrep: gosec.G101-1

	_, _, _ = apiKeyHeader, headerPassword, passwordEnvVar
}

// --- negatives: shapes the rule must not flag ---

// An empty string cannot be a credential; table-driven tests use it to assert
// that a required setting was left unset.
func emptyIsNotACredential() {
	password := "" // nosemgrep: gosec.G101-1
	_ = password
}

// A name without a credential word is out of scope however secret-looking the
// value is. Entropy is gosec's job, and the whole reason this rule exists is
// that entropy filtering is what let internal/redact's own "s3cret" and
// "sup3r-s3cret-token" fixtures through unreported.
func unrelatedName() {
	spanID := "sup3rs3cr3t"
	_ = spanID
}

// A value read from the environment is the shape the rule is steering toward,
// so it must stay silent even under a credential-shaped name.
func fromEnvIsFine(lookup func(string) string) {
	password := lookup("GRAPH_PROXY_PASSWORD")
	_ = password
}
