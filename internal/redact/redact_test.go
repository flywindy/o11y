package redact_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flywindy/o11y/internal/redact"
)

// TestURL covers the endpoint shapes the SDK may be handed, including the
// ones where userinfo must not survive into a log line.
func TestURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", ""},
		{"no credentials", "http://alloy.infra.svc.cluster.local:4040", "http://alloy.infra.svc.cluster.local:4040"},
		{"user and password", "http://user:s3cret@pyroscope:4040", "http://redacted@pyroscope:4040"},
		{"user only", "http://tenant-a@pyroscope:4040", "http://redacted@pyroscope:4040"},
		{"password with url-unsafe bytes", "https://u:p%40ss%2Fword@host/path", "https://redacted@host/path"},
		{"credentials with a query string", "http://u:p@host:4040/ingest?name=svc", "http://redacted@host:4040/ingest?name=svc"},
		{"host-port only, no scheme", "pyroscope:4040", "pyroscope:4040"},
		{"unparseable but contains an @", "http://u:p@host:4040/\x7f\x00", "[endpoint redacted]"},
		// url.Parse accepts these without error and reports no userinfo, so
		// returning the input verbatim would print the credential in full.
		{"opaque url hides the credential", "http:user:s3cret@host", "[endpoint redacted]"},
		{"scheme-less opaque url", "user:s3cret@host:4040", "[endpoint redacted]"},
		{"non-http opaque url", "mailto:user:s3cret@host", "[endpoint redacted]"},
		// "@" the parser positively attributed to a path or query is not
		// userinfo, so the endpoint stays legible.
		{"at-sign in the path", "http://pyroscope:4040/ingest@v1", "http://pyroscope:4040/ingest@v1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, redact.URL(tt.raw))
		})
	}
}

// TestURLNeverLeaksThePassword is the property that matters for URL: whatever
// shape the endpoint takes, the secret must not survive into the logged string.
func TestURLNeverLeaksThePassword(t *testing.T) {
	// Fixture credential asserted absent from redact.URL's output below, not a
	// live credential. Both ids are named because each scanner matches only
	// its own: gosec.G101-1 is the registry rule an external scan reports,
	// hardcoded-credential-literal is this repo's own.
	// nosemgrep: gosec.G101-1, hardcoded-credential-literal
	const secret = "sup3r-s3cret-token"
	for _, raw := range []string{
		"http://user:" + secret + "@pyroscope:4040",
		"https://user:" + secret + "@pyroscope:4040/ingest?name=svc",
		"http://" + secret + "@pyroscope:4040",
		"http://user:" + secret + "@pyroscope:4040/\x7f",
		// Opaque forms: url.Parse reports no userinfo for any of these.
		"http:user:" + secret + "@pyroscope",
		"user:" + secret + "@pyroscope:4040",
		"mailto:user:" + secret + "@host",
		"//user:" + secret + "@pyroscope:4040",
	} {
		assert.NotContains(t, redact.URL(raw), secret, "input %q leaked its credential", raw)
	}
}

// TestInText covers the second half of the leak: redacting the endpoint
// attribute is pointless if the error logged beside it quotes the endpoint back.
// net/url renders a parse failure as `parse "<raw>": …`, and Pyroscope's client
// returns that error verbatim.
func TestInText(t *testing.T) {
	// Fixture credential embedded in a malformed URL, not a live one. gosec
	// reports this shape (password-in-URL) even though its entropy filter
	// misses the bare `const secret` above, so this line needs the gosec
	// directive the other does not.
	// #nosec G101 -- fabricated fixture endpoint, not a live credential
	// nosemgrep: gosec.G101-1
	const endpoint = "http://user:s3cret%zz@pyroscope:4040"
	// The shape net/url actually produces, confirmed against the stdlib.
	text := `parse "` + endpoint + `": invalid URL escape "%zz"`

	got := redact.InText(text, endpoint)

	assert.NotContains(t, got, "s3cret", "the credential must not survive into the logged error")
	assert.Contains(t, got, "invalid URL escape", "the diagnostic part must survive")
}

// TestInTextRedactsEndpointsItWasNotToldAbout is the case that makes the scrub
// generic rather than keyed on the configured endpoint: pyroscope-go replaces
// the address from PYROSCOPE_ADHOC_SERVER_ADDRESS before parsing it, so the
// endpoint quoted in the error can be one the SDK never configured.
func TestInTextRedactsEndpointsItWasNotToldAbout(t *testing.T) {
	const configured = "http://alloy.infra.svc.cluster.local:4040"
	// #nosec G101 -- fabricated fixture endpoint, not a live credential
	// nosemgrep: gosec.G101-1
	const override = "http://user:s3cret%zz@adhoc-host:4040"
	text := `parse "` + override + `": invalid URL escape "%zz"`

	got := redact.InText(text, configured)

	assert.NotContains(t, got, "s3cret",
		"an endpoint the SDK never configured must still have its credential scrubbed")
	assert.Contains(t, got, "adhoc-host", "the host stays, so the failure is still diagnosable")
}

// TestInTextClosesTheShapesPatternsMiss covers the forms that successive review
// rounds found slipping past pattern matching. None of them is handled by a
// dedicated pattern; all are caught by the closing rule that no "@" may survive.
func TestInTextClosesTheShapesPatternsMiss(t *testing.T) {
	// Fixture credential asserted absent from redact.InText's output below, not
	// a live credential.
	// nosemgrep: gosec.G101-1, hardcoded-credential-literal
	const secret = "s3cret"
	tests := []struct {
		name string
		text string
	}{
		{"escaped quote in userinfo", `parse "http://user:sec\"` + secret + `@host": net/url: invalid userinfo`},
		{"space in userinfo", `parse "http://user:sec ` + secret + `@host": net/url: invalid userinfo`},
		{"opaque url, not a known endpoint", `parse "http:user:` + secret + `@host": bad`},
		{"scheme-relative url", `parse "//user:` + secret + `@host": bad`},
		{"no scheme at all", `dial user:` + secret + `@host:4040: refused`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// No knownEndpoints: the caller cannot know the overridden value.
			got := redact.InText(tt.text)
			assert.NotContains(t, got, secret, "credential survived: %s", got)
		})
	}
}

func TestInTextLeavesUnrelatedTextAlone(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		endpoint string
		want     string
	}{
		{"endpoint absent", "connection refused", "http://u:p@host", "connection refused"},
		{"empty endpoint", "some error", "", "some error"},
		{"no credential anywhere", `dial "http://pyroscope:4040": refused`, "http://pyroscope:4040", `dial "http://pyroscope:4040": refused`},
		{"every occurrence replaced", "http://u:p@h and http://u:p@h", "http://u:p@h", "http://redacted@h and http://redacted@h"},
		{"opaque form needs the known endpoint", `parse "http:u:p@h": bad`, "http:u:p@h", `parse "[endpoint redacted]": bad`},
		// The cost of the closing rule: text keeping an "@" for an innocent
		// reason is sacrificed rather than reasoned about.
		{"an unrelated at-sign is sacrificed to the rule", "notify ops@example.com on failure", "", "[endpoint redacted]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, redact.InText(tt.text, tt.endpoint))
		})
	}
}

// TestSecrets checks opaque secrets are replaced wherever they occur, the
// longest first so a whole header string and its parts coexist, and that
// empty secrets and an empty list are no-ops.
func TestSecrets(t *testing.T) {
	const raw = "authorization=BearerSecret%zz, x-api-key=k-1234567890"
	got := redact.Secrets(`escape header value: value="BearerSecret%zz" in `+raw, raw, "BearerSecret%zz", "", "k-1234567890")
	assert.NotContains(t, got, "BearerSecret")
	assert.NotContains(t, got, "k-1234567890")
	assert.Equal(t, `escape header value: value="[redacted]" in [redacted]`, got, "the whole string is replaced as one, the loose copy on its own")

	assert.Equal(t, "value=[redacted] at 2026-09-13T11:00 attempt 12, x=[redacted]; [redacted]",
		redact.Secrets("value=1 at 2026-09-13T11:00 attempt 12, x=ab; 1", "1", "ab"),
		"a short secret is replaced only where it is a whole token")
	assert.Equal(t, "tab table", redact.Secrets("tab table", "ab"), "a short secret inside a word is left alone")

	assert.Equal(t, "plain", redact.Secrets("plain"))
	assert.Equal(t, "plain", redact.Secrets("plain", ""))
	assert.Equal(t, "", redact.Secrets("", "x"))
}

// TestURLAttribute pins what a url.full span attribute may carry. A span
// attribute leaves the process exactly as a log record does, so semconv
// v1.39.0's rule — redact userinfo, scrub the credential query parameters —
// is the same safety property the rest of this package enforces.
func TestURLAttribute(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "plain URL is untouched",
			raw:  "https://api.example.com/orders?page=2&sort=asc",
			want: "https://api.example.com/orders?page=2&sort=asc",
		},
		{
			name: "userinfo is replaced, the rest stays",
			// #nosec G101 -- fabricated fixture URL, not a live credential
			// nosemgrep: gosec.G101-1
			raw:  "http://bob:hunter2@127.0.0.1:8080/orders?page=2",
			want: "http://redacted@127.0.0.1:8080/orders?page=2",
		},
		{
			name: "a username with no password is still userinfo",
			raw:  "https://bob@api.example.com/orders",
			want: "https://redacted@api.example.com/orders",
		},
		{
			name: "the AWS presigned parameters semconv names are scrubbed",
			raw:  "https://s3.example.com/b/k?AWSAccessKeyId=AKIAIOSFODNN7&Expires=1700000000&Signature=abc%2Bdef",
			want: "https://s3.example.com/b/k?AWSAccessKeyId=%5Bredacted%5D&Expires=1700000000&Signature=%5Bredacted%5D",
		},
		{
			name: "so are the short and Google forms, whatever their case",
			raw:  "https://storage.example.com/o?sig=xyz&X-GOOG-SIGNATURE=abc&name=report",
			want: "https://storage.example.com/o?sig=%5Bredacted%5D&X-GOOG-SIGNATURE=%5Bredacted%5D&name=report",
		},
		{
			name: "parameter order and escaping are preserved",
			raw:  "https://api.example.com/x?z=1&a=hello%20world&Signature=s",
			want: "https://api.example.com/x?z=1&a=hello%20world&Signature=%5Bredacted%5D",
		},
		{
			name: "a bare flag that happens to match a key has no value to scrub",
			raw:  "https://api.example.com/x?sig&page=1",
			want: "https://api.example.com/x?sig&page=1",
		},
		{
			name: "a key that is not a credential keeps its value",
			raw:  "https://api.example.com/x?signature_version=4",
			want: "https://api.example.com/x?signature_version=4",
		},
		{
			name: "userinfo and a signature together",
			// #nosec G101 -- fabricated fixture URL, not a live credential
			// nosemgrep: gosec.G101-1
			raw:  "https://bob:hunter2@s3.example.com/b/k?Signature=abc",
			want: "https://redacted@s3.example.com/b/k?Signature=%5Bredacted%5D",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.raw)
			require.NoError(t, err)

			got := redact.URLAttribute(u)

			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.raw, u.String(), "the caller's URL must not be modified")
		})
	}
}

// TestURLAttribute_NilIsEmpty pins that a caller with no URL gets an empty
// attribute rather than a panic; the resty hook reaches targetFromURL before
// resty has built a RawRequest on some paths.
func TestURLAttribute_NilIsEmpty(t *testing.T) {
	assert.Empty(t, redact.URLAttribute(nil))
}

// TestURLAttribute_FailsClosedOnUnaccountedCredentials pins that a URL whose
// credentials url.Parse does not attribute to userinfo is replaced wholesale
// rather than rendered back out.
//
// url.Parse only recognises userinfo in a hierarchical URL. Given an opaque
// one it leaves the credential in Opaque with User nil, so replacing User
// alone would emit the secret unchanged. This is the same closed rule
// redact.URL applies, and it is why the span still carries server.address and
// server.port separately: losing url.full does not lose the destination.
func TestURLAttribute_FailsClosedOnUnaccountedCredentials(t *testing.T) {
	for _, raw := range []string{
		// #nosec G101 -- fabricated fixture URL, not a live credential
		// nosemgrep: gosec.G101-1
		"http:alice:secret@collector",
		// #nosec G101 -- fabricated fixture URL, not a live credential
		// nosemgrep: gosec.G101-1
		"mailto:alice:secret@collector",
	} {
		t.Run(raw, func(t *testing.T) {
			u, err := url.Parse(raw)
			require.NoError(t, err)
			require.Nil(t, u.User, "the premise: the parser did not attribute this as userinfo")

			got := redact.URLAttribute(u)

			assert.NotContains(t, got, "secret")
			assert.Equal(t, "[endpoint redacted]", got)
		})
	}
}

// TestGoEscaped covers the rendering a secret takes wherever something prints
// it with %q, which is the second form every secret list has to carry.
func TestGoEscaped(t *testing.T) {
	assert.Equal(t, "plain", redact.GoEscaped("plain"))
	assert.Equal(t, `tab\there`, redact.GoEscaped("tab\there"))
	assert.Equal(t, `say \"hi\"`, redact.GoEscaped(`say "hi"`))
	assert.Equal(t, `\x00`, redact.GoEscaped("\x00"))
}

// TestInText_DoesNotTrustAPreExistingPlaceholder pins that text which already
// contains this package's own placeholder cannot talk InText out of its closed
// rule.
//
// The scheme-relative form is the one that matters: the userinfo pattern
// anchors on "://" and does not match it, so nothing is substituted, and a
// check that stripped "redacted@" from the finished text would strip the
// input's own — leaving no "@" to fail on and returning the credential beside
// it verbatim.
func TestInText_DoesNotTrustAPreExistingPlaceholder(t *testing.T) {
	// #nosec G101 -- fabricated fixture URL, not a live credential
	// nosemgrep: gosec.G101-1
	const message = "uploading at //alice:redacted@pyroscope:4040/ingest"

	got := redact.InText(message)

	assert.NotContains(t, got, "alice", "a placeholder in the input is not evidence the input is safe")
	assert.Equal(t, "[endpoint redacted]", got)
}

// TestInText_StillRendersItsOwnSubstitutions pins the other half: a hierarchical
// URL the pattern does match is still rendered readably, rather than being
// discarded by the same rule.
func TestInText_StillRendersItsOwnSubstitutions(t *testing.T) {
	// #nosec G101 -- fabricated fixture URL, not a live credential
	// nosemgrep: gosec.G101-1
	const message = "uploading at http://alice:s3cret@pyroscope:4040/ingest?name=svc"

	got := redact.InText(message)

	assert.Equal(t, "uploading at http://redacted@pyroscope:4040/ingest?name=svc", got)
}

// TestErrorText_SurvivesABrokenError pins that rendering an error the SDK did
// not construct cannot take the process down. A diagnostic is never worth a
// crash, and Error is reached from Shutdown, where a panic would also skip
// every closer still to run.
func TestErrorText_SurvivesABrokenError(t *testing.T) {
	var typedNil *panickingError
	assert.Equal(t, "<nil *redact_test.panickingError>", redact.ErrorText(typedNil))
	assert.Equal(t, "[omitted: *redact_test.panickingError panicked while rendering]",
		redact.ErrorText(&panickingError{}))
	assert.Equal(t, "ordinary", redact.ErrorText(errors.New("ordinary")))
}

// TestError_SurvivesABrokenError pins that Error renders through the same guard
// rather than calling the dependency's Error method itself, and that it hands
// back something the caller can render again.
//
// The second part is the one that matters. A placeholder can equal the redacted
// message, so an "unchanged, return the original" shortcut would give the
// caller back the very value whose Error method panics — and every caller here
// renders the result: resty into the span's status description and its
// exception event, Shutdown into its log record.
func TestError_SurvivesABrokenError(t *testing.T) {
	var typedNil *panickingError

	for name, broken := range map[string]error{
		"typed nil": typedNil,
		"panicking": &panickingError{},
	} {
		t.Run(name, func(t *testing.T) {
			var got error
			require.NotPanics(t, func() { got = redact.Error(broken, nil, nil) })
			require.NotNil(t, got)

			var text string
			require.NotPanics(t, func() { text = got.Error() },
				"the caller renders what Error hands back")
			assert.NotEmpty(t, text)
			assert.ErrorIs(t, got, broken, "the chain still reaches the original")
		})
	}
}

// TestError_RedactsEveryURLInTheChain pins the two ways a URL reaches an error
// message that substituting the outermost one alone would miss.
//
// net/http wraps a transport error in an outer *url.Error, so a RoundTripper
// that performs a request of its own leaves a second one nested inside with its
// own URL. And url.Error.Error formats the field with %q, so a URL holding a
// character the quoting escapes appears in the message only escaped.
func TestError_RedactsEveryURLInTheChain(t *testing.T) {
	t.Run("nested url.Error with an unparseable URL fails closed", func(t *testing.T) {
		// #nosec G101 -- fabricated fixture URL, not a live credential
		// nosemgrep: gosec.G101-1
		inner := &url.Error{Op: "Get", URL: "https://host/%zz?Signature=s3cret", Err: errors.New("dial")}
		outer := &url.Error{Op: "Get", URL: "https://host/x", Err: inner}

		got := redact.Error(outer, nil, nil).Error()

		assert.NotContains(t, got, "s3cret",
			"a signed query carries its credential with no @, so the closed rule cannot catch it")
		assert.Equal(t, "[endpoint redacted]", got)
	})

	t.Run("nested url.Error with a parseable URL is redacted, not discarded", func(t *testing.T) {
		// #nosec G101 -- fabricated fixture URL, not a live credential
		// nosemgrep: gosec.G101-1
		inner := &url.Error{Op: "Get", URL: "https://host/y?Signature=s3cret", Err: errors.New("dial")}
		outer := &url.Error{Op: "Get", URL: "https://host/x", Err: inner}

		got := redact.Error(outer, nil, nil).Error()

		assert.NotContains(t, got, "s3cret")
		assert.Contains(t, got, "host/y", "the rest of the message survives")
		assert.Contains(t, got, "dial")
	})

	t.Run("a URL that appears only in its quoted form is still replaced", func(t *testing.T) {
		// #nosec G101 -- fabricated fixture URL, not a live credential
		// nosemgrep: gosec.G101-1
		const raw = "https://host/x?Signature=s3cret&note=\"q\""
		wrapped := &url.Error{Op: "Get", URL: raw, Err: errors.New("dial")}
		require.NotContains(t, wrapped.Error(), raw,
			"the premise: %q escapes the quote, so the raw form is not in the message")

		got := redact.Error(wrapped, nil, nil).Error()

		assert.NotContains(t, got, "s3cret")
	})
}

// panickingError stands in for a dependency's error whose Error method is
// broken — a shape the SDK cannot rule out in a third-party exporter.
type panickingError struct{}

func (e *panickingError) Error() string { panic("boom") }

// TestError_RedactsOverlappingNestedURLs pins the order the substitutions run
// in. A nested *url.Error commonly holds the outer one's URL plus a query, so
// replacing the outer first rewrites its prefix inside the inner occurrence
// too — and the inner URL, no longer matching, keeps whatever its query
// carried. Worse, the message then holds an accounted-for "redacted@", so the
// closed rule sees nothing wrong with it.
func TestError_RedactsOverlappingNestedURLs(t *testing.T) {
	// #nosec G101 -- fabricated fixture URL, not a live credential
	// nosemgrep: gosec.G101-1
	const outerURL = "https://alice@host/x"
	// #nosec G101 -- fabricated fixture URL, not a live credential
	// nosemgrep: gosec.G101-1
	const innerURL = outerURL + "?Signature=s3cret"
	require.Contains(t, innerURL, outerURL, "the premise: one URL is a prefix of the other")

	inner := &url.Error{Op: "Get", URL: innerURL, Err: errors.New("dial")}
	outer := &url.Error{Op: "Get", URL: outerURL, Err: inner}

	got := redact.Error(outer, nil, nil).Error()

	assert.NotContains(t, got, "s3cret")
	assert.NotContains(t, got, "alice")
	assert.Contains(t, got, "host/x", "the rest of the message survives")
}

// TestInText_CountsRatherThanRewriting pins that text already holding this
// package's placeholder — in either of the two shapes that have fooled this
// check before — cannot talk it out of the closed rule.
//
// The rule is checked by counting at-signs against the matches that account for
// them, so nothing the function emits can be mistaken for something the input
// brought, and nothing the input brings can be mistaken for something the
// function emitted.
func TestInText_CountsRatherThanRewriting(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "a placeholder in the input is not evidence the input is safe",
			// #nosec G101 -- fabricated fixture URL, not a live credential
			// nosemgrep: gosec.G101-1
			text: "uploading at //alice:redacted@pyroscope:4040/ingest",
			want: "[endpoint redacted]",
		},
		{
			name: "an unmatched at-sign beside a matched one still fails the rule",
			// #nosec G101 -- fabricated fixture URL, not a live credential
			// nosemgrep: gosec.G101-1
			text: "http://alice:s3cret@host/a and //bob:pw@host/b",
			want: "[endpoint redacted]",
		},
		{
			// The NUL-delimited marker a previous implementation of this check
			// substituted. Text carrying it had it swapped for a "redacted@"
			// the check had never accounted for, which is what made a fixed
			// marker the wrong mechanism rather than the wrong marker.
			name: "a previous implementation's marker in the input is inert",
			// #nosec G101 -- fabricated fixture text, not a live credential
			// nosemgrep: gosec.G101-1
			text: "http://alice:s3cret\x00userinfo\x00host",
			want: "http://alice:s3cret\x00userinfo\x00host",
		},
		{
			name: "two hierarchical URLs are both accounted for",
			// #nosec G101 -- fabricated fixture URL, not a live credential
			// nosemgrep: gosec.G101-1
			text: "http://alice:s3cret@host/a and https://bob:pw@host/b",
			want: "http://redacted@host/a and https://redacted@host/b",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := redact.InText(tt.text)

			// The exact output is the assertion; a blanket "must not contain"
			// would be wrong for the inert-marker case, where the text has no
			// "@" at all and so declares no userinfo for the rule to act on —
			// what matters there is that nothing is added to make it look like
			// it did.
			assert.Equal(t, tt.want, got)
			assert.NotContains(t, got, "s3cret@", "no credential may survive as userinfo")
		})
	}
}

// TestError_SurvivesAPanickingUnwrap pins that a dependency's Unwrap is guarded
// the same way its Error is. Shutdown reaches this, and a panic there would
// skip every closer still to run.
func TestError_SurvivesAPanickingUnwrap(t *testing.T) {
	broken := panickingUnwrap{}

	var got error
	require.NotPanics(t, func() { got = redact.Error(broken, nil, nil) })
	require.NotNil(t, got)
	assert.Equal(t, "safe message", got.Error(), "the error rendered itself, so its own text stands")
}

// panickingUnwrap stands in for a dependency's error that renders cleanly but
// whose Unwrap is broken — a shape the guard on Error alone does not cover.
type panickingUnwrap struct{}

func (panickingUnwrap) Error() string { return "safe message" }
func (panickingUnwrap) Unwrap() error { panic("boom") }

// TestURL_RedactsASignedQuery pins that redact.URL applies the same
// credential-query rule URLAttribute does.
//
// The two used to be separate copies of one rule, and the copy in URL only knew
// about userinfo. So an endpoint configured as a presigned URL was returned
// verbatim by the very function whose job is to make an endpoint safe to log —
// including by InText, which substitutes URL(endpoint) wherever it finds the
// configured value.
func TestURL_RedactsASignedQuery(t *testing.T) {
	// #nosec G101 -- fabricated fixture URL, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const signed = "https://pyroscope:4040/ingest?Signature=s3cret&name=svc"

	got := redact.URL(signed)

	assert.NotContains(t, got, "s3cret")
	assert.Contains(t, got, "name=svc", "the rest of the query is not a credential")
}

// TestURL_LeavesAnOrdinaryEndpointExactlyAsConfigured pins the other half: the
// new check must not start rewriting endpoints that carry nothing.
func TestURL_LeavesAnOrdinaryEndpointExactlyAsConfigured(t *testing.T) {
	const plain = "http://collector:4318/v1/traces?compression=gzip"

	assert.Equal(t, plain, redact.URL(plain))
}

// TestInText_FailsClosedOnASignedURLItCannotAccountFor pins InText's second
// closed rule.
//
// The first rule holds on "@", which a presigned URL does not have: its
// credential is a query value. The case that found this is a signed profiling
// endpoint — pyroscope's uploader appends "/ingest" and re-encodes the query
// before formatting the URL into "uploading at %s", so the configured endpoint
// is no longer a substring for InText to substitute, and the signature reached
// a DEBUG line on every upload and an ERROR line on every failure.
func TestInText_FailsClosedOnASignedURLItCannotAccountFor(t *testing.T) {
	// #nosec G101 -- fabricated fixture endpoint, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const endpoint = "https://pyroscope:4040?Signature=s3cret"
	// What the uploader actually formats: path joined, query re-encoded, so
	// the configured string does not appear.
	// #nosec G101 -- fabricated fixture endpoint, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const line = "uploading at https://pyroscope:4040/ingest?Signature=s3cret&from=1&name=svc"
	require.NotContains(t, line, endpoint, "the premise: the endpoint is not a substring of the message")

	got := redact.InText(line, endpoint)

	assert.NotContains(t, got, "s3cret")
	assert.Equal(t, "[endpoint redacted]", got)
}

// TestInText_AcceptsACredentialQueryItRedactedItself pins that the rule does
// not now discard a message InText legitimately made safe: where the configured
// endpoint *is* a substring, URL replaces the signature and the result must
// still read.
func TestInText_AcceptsACredentialQueryItRedactedItself(t *testing.T) {
	// #nosec G101 -- fabricated fixture endpoint, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const endpoint = "https://pyroscope:4040/ingest?Signature=s3cret&name=svc"

	got := redact.InText("uploading at "+endpoint, endpoint)

	assert.NotContains(t, got, "s3cret")
	assert.Contains(t, got, "uploading at https://pyroscope:4040/ingest?Signature=")
	assert.Contains(t, got, "name=svc")
}

// TestInText_DoesNotTrustAForgedCredentialPlaceholder pins that the accepted
// form is the placeholder in full.
//
// The rule accepts a credential query parameter whose value is exactly what
// redactQuery writes. A value that merely starts with it is a different value,
// and the round that introduced a fixed sentinel into this package learned what
// happens when input can dress itself up as the function's own output.
func TestInText_DoesNotTrustAForgedCredentialPlaceholder(t *testing.T) {
	// #nosec G101 -- fabricated fixture URL, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const forged = "uploading at https://host/ingest?Signature=%5Bredacted%5Ds3cret"

	got := redact.InText(forged)

	assert.NotContains(t, got, "s3cret")
	assert.Equal(t, "[endpoint redacted]", got)
}

// cyclicMultiError unwraps into itself twice, the shape that turns a depth cap
// into no cap at all.
type cyclicMultiError struct{ subs []error }

func (e *cyclicMultiError) Error() string   { return "cyclic" }
func (e *cyclicMultiError) Unwrap() []error { return e.subs }

// TestError_TerminatesOnACyclicMultiError pins that the walk over an error
// chain is bounded by the errors it visits, not by how deep it goes.
//
// A depth cap alone bounds nothing once Unwrap returns a slice: an error
// holding itself twice branches in two at every step, so a cap of 64 permits
// on the order of 2^65 visits. SDK.Shutdown calls this synchronously, ahead of
// the closers still to run, so an error from a dependency must not be able to
// hold it there. Before the fix this test does not fail — it hangs.
func TestError_TerminatesOnACyclicMultiError(t *testing.T) {
	cyclic := &cyclicMultiError{}
	cyclic.subs = []error{cyclic, cyclic}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = redact.Error(cyclic, nil, nil)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("redact.Error did not return: the walk over the chain is unbounded")
	}
}

// TestHeaderWireValue covers the form net/http actually sends, which is the
// third rendering of a secret every secret list has to carry.
//
// Header.Set stores a value untouched, but the write path replaces CR and LF
// with spaces and trims surrounding ASCII whitespace, so a server that echoes
// the header it received reports a string the configured value does not match.
func TestHeaderWireValue(t *testing.T) {
	assert.Equal(t, "Bearer token", redact.HeaderWireValue(" Bearer token "))
	assert.Equal(t, "Bearer token", redact.HeaderWireValue("\tBearer token\t"))
	assert.Equal(t, "Bearer token", redact.HeaderWireValue("\r\nBearer token\r\n"))
	assert.Equal(t, "Bearer token", redact.HeaderWireValue("Bearer token"))
	assert.Empty(t, redact.HeaderWireValue("   "))
}

// TestHeaderWireValue_MatchesWhatNetHTTPSends pins the claim the function is
// built on against net/http itself, rather than against a reading of it: the
// value a server receives is HeaderWireValue's, not the configured string.
//
// The cases are the ones a request can actually carry. A value holding CR or LF
// is rejected by Transport before anything is written, so there is no wire form
// of it to match.
func TestHeaderWireValue_MatchesWhatNetHTTPSends(t *testing.T) {
	for _, configured := range []string{
		" Bearer token ",
		"\tBearer token\t",
		"Bearer token",
	} {
		t.Run(redact.GoEscaped(configured), func(t *testing.T) {
			var received string
			srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				received = r.Header.Get("Authorization")
			}))
			defer srv.Close()

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", configured)
			resp, err := srv.Client().Do(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())

			assert.Equal(t, received, redact.HeaderWireValue(configured))
		})
	}
}

// TestCredentialQueryKeysAreDecodedBeforeComparison pins that a
// percent-encoded credential key is recognised everywhere, not only where the
// query is parsed.
//
// A query key is percent-encoded text and net/url decodes it before anything
// reads it — `url.parseQuery` runs `QueryUnescape` on the key, not only on the
// value — so `?Sign%61ture=` is the parameter `Signature` to every server and
// to `url.Values`. `redactQuery` always decoded; the pattern that `URL`'s fast
// path and `InText`'s rule were built on matched literal spellings, so the two
// disagreed about the same URL and the encoded form went through both of them
// untouched:
//
//	URL          = https://pyroscope:4040/ingest?Sign%61ture=s3cret
//	URLAttribute = https://pyroscope:4040/ingest?Sign%61ture=%5Bredacted%5D
//
// One decision now serves both.
func TestCredentialQueryKeysAreDecodedBeforeComparison(t *testing.T) {
	// #nosec G101 -- fabricated fixture URL, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const encoded = "https://pyroscope:4040/ingest?Sign%61ture=s3cret&name=svc"

	u, err := url.Parse(encoded)
	require.NoError(t, err)
	require.Equal(t, []string{"s3cret"}, u.Query()["Signature"],
		"the premise: net/url decodes the key, so this is a Signature parameter")

	t.Run("URL", func(t *testing.T) {
		got := redact.URL(encoded)
		assert.NotContains(t, got, "s3cret")
		assert.Contains(t, got, "name=svc")
	})

	t.Run("InText", func(t *testing.T) {
		got := redact.InText("uploading at " + encoded)
		assert.NotContains(t, got, "s3cret")
	})

	t.Run("InText with the endpoint declared", func(t *testing.T) {
		got := redact.InText(`parse "`+encoded+`": bad`, encoded)
		assert.NotContains(t, got, "s3cret")
	})

	t.Run("URLAttribute", func(t *testing.T) {
		got := redact.URLAttribute(u)
		assert.NotContains(t, got, "s3cret")
	})
}

// TestQueryKeysThatOnlyLookLikeCredentials pins the other side: the wider
// pattern must not start failing closed on ordinary parameters.
func TestQueryKeysThatOnlyLookLikeCredentials(t *testing.T) {
	for _, raw := range []string{
		"https://host/p?signature_version=4&name=svc",
		"https://host/p?design=flat",
		"https://host/p?a=1&b=2",
	} {
		t.Run(raw, func(t *testing.T) {
			assert.Equal(t, raw, redact.URL(raw), "nothing here is a credential key")
			assert.Equal(t, "at "+raw, redact.InText("at "+raw))
		})
	}
}

// TestJSONEscaped covers the rendering a secret takes in a JSON document, the
// third form every secret list has to carry.
//
// It is not GoEscaped with different punctuation. encoding/json escapes "<",
// ">" and "&" as <, > and & unless a caller turns SetEscapeHTML
// off, and renders a control byte as \u0000 where %q renders \x00. A Pyroscope
// or OpenTelemetry Collector deployment is a Go program putting an error into a
// JSON body, so this is what a server echoing a received header produces.
func TestJSONEscaped(t *testing.T) {
	assert.Equal(t, `plain`, redact.JSONEscaped("plain"))
	assert.Equal(t, `tok\u003cen\u0026more\u003e`, redact.JSONEscaped("tok<en&more>"))
	assert.Equal(t, `say \"hi\"`, redact.JSONEscaped(`say "hi"`))
	assert.Equal(t, `a\tb`, redact.JSONEscaped("a\tb"))
	assert.Equal(t, `\u0000`, redact.JSONEscaped("\x00"), "%q would render this as \\x00")
}

// TestJSONEscaped_MatchesWhatAGoServerWrites pins the function against
// encoding/json itself rather than against a reading of it, the same way the
// wire form is pinned against net/http.
func TestJSONEscaped_MatchesWhatAGoServerWrites(t *testing.T) {
	// #nosec G101 -- fabricated fixture header value, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const token = "Bearer a<b&c>d"

	body, err := json.Marshal(struct {
		Error string `json:"error"`
	}{Error: "rejected Authorization: " + token})
	require.NoError(t, err)
	require.NotContains(t, string(body), token, "the premise: the raw form is not in the body")

	assert.Contains(t, string(body), redact.JSONEscaped(token))
}

// TestRenderings pins the one place that decides which forms of a secret are
// listed, and that a value the escaping leaves alone is listed once.
func TestRenderings(t *testing.T) {
	assert.Equal(t, []string{"plain"}, redact.Renderings("plain"))
	assert.Equal(t, []string{
		"tok<en&more>",
		`tok\u003cen\u0026more\u003e`,
		"tok&lt;en&amp;more&gt;",
		`tok\\u003cen\\u0026more\\u003e`,
		`tok\u0026lt;en\u0026amp;more\u0026gt;`,
		"tok&amp;lt;en&amp;amp;more&amp;gt;",
	}, redact.Renderings("tok<en&more>"),
		"%q leaves these alone; JSON and HTML each escape them their own way, "+
			"and each escaping of an escaped form is listed too")
	assert.Equal(t, []string{
		"a\tb", `a\tb`, `a\\tb`,
	}, redact.Renderings("a\tb"),
		"%q and JSON agree here, so the first round adds one form rather than two")
}

// TestRenderings_ComposesTheEscapings pins the composition itself, on the
// shape that produces it: a server that HTML-escapes the value it echoes and
// then writes that string into a JSON error body.
//
// The composed form matches none of the three single escapings — that is the
// whole point of listing it — so the assertion is on the form, not merely on
// the length of the list.
func TestRenderings_ComposesTheEscapings(t *testing.T) {
	// #nosec G101 -- fabricated fixture header value, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const token = "tok&en"

	composed := redact.JSONEscaped(redact.HTMLEscaped(token))
	require.Equal(t, `tok\u0026amp;en`, composed, "the premise: this is what such a server writes")
	for _, single := range []string{token, redact.GoEscaped(token), redact.JSONEscaped(token), redact.HTMLEscaped(token)} {
		require.NotEqual(t, composed, single, "and no single escaping produces it")
	}

	assert.Contains(t, redact.Renderings(token), composed)

	body := `{"error":"rejected header ` + composed + `"}`
	assert.NotContains(t, redact.Secrets(body, redact.Renderings(token)...), composed,
		"so Secrets can replace it in place rather than give up the line")
}

// TestSecrets_RedactsAJSONEscapedValue is the end-to-end shape: a credential
// echoed back inside a Go server's JSON error body, scrubbed with the list
// Renderings built.
func TestSecrets_RedactsAJSONEscapedValue(t *testing.T) {
	// #nosec G101 -- fabricated fixture header value, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const token = "Bearer a<b&c>d"

	body, err := json.Marshal(struct {
		Error string `json:"error"`
	}{Error: "rejected Authorization: " + token})
	require.NoError(t, err)

	got := redact.Secrets(string(body), redact.Renderings(token)...)

	assert.NotContains(t, got, `a\u003cb\u0026c\u003ed`)
	assert.Contains(t, got, "rejected Authorization:", "the rest of the body survives")
}

// TestURL_FailsClosedOnACredentialShapedOpaqueURL pins the gap the fast path
// left open.
//
// url.Parse attributes nothing in an opaque URL, so "http:alice:secret" has no
// userinfo and no "@" — the closed rule never engaged, the fast path returned
// the string verbatim, and URLAttribute, which does inspect Opaque, was never
// reached. The earlier opaque cases in TestURL all happen to contain an "@",
// which is why they passed.
func TestURL_FailsClosedOnACredentialShapedOpaqueURL(t *testing.T) {
	for _, raw := range []string{
		// #nosec G101 -- fabricated fixture URL, not a live credential
		// nosemgrep: hardcoded-credential-literal,gosec.G101-1
		"http:alice:s3cret",
		// #nosec G101 -- fabricated fixture URL, not a live credential
		// nosemgrep: hardcoded-credential-literal,gosec.G101-1
		"pyroscope:alice:s3cret",
	} {
		t.Run(raw, func(t *testing.T) {
			u, err := url.Parse(raw)
			require.NoError(t, err)
			require.Nil(t, u.User, "the premise: the parser attributed no userinfo")
			require.NotContains(t, raw, "@", "and there is no at-sign for the other rule to catch")

			assert.Equal(t, "[endpoint redacted]", redact.URL(raw))
			assert.Equal(t, "[endpoint redacted]", redact.URLAttribute(u))
		})
	}
}

// TestURL_KeepsAHostPortEndpointLegible pins the other side of that rule.
//
// url.Parse reads "pyroscope:4040" as the scheme "pyroscope" with the opaque
// payload "4040" — a single token, which cannot be a "user:pass" pair. Such an
// endpoint does not actually work in this SDK (url.URL.String() ignores Path
// when Opaque is set, so the exporters' path defaults are dropped), which is
// exactly when an operator most needs the log line to say what they configured.
func TestURL_KeepsAHostPortEndpointLegible(t *testing.T) {
	for _, raw := range []string{"pyroscope:4040", "collector:4318/v1/traces"} {
		t.Run(raw, func(t *testing.T) {
			u, err := url.Parse(raw)
			require.NoError(t, err)
			require.NotEmpty(t, u.Opaque, "the premise: this parses as an opaque URL")

			assert.Equal(t, raw, redact.URL(raw))
		})
	}
}

// TestHeaderWireNameHTTP2 covers the form an h2 peer receives a header name in.
//
// net/http does not send the canonical spelling over HTTP/2: it encodes each
// name through httpcommon.LowerHeader, and a TLS endpoint negotiates h2 by
// default, so the same request that sends "X-Api-Key" over HTTP/1.1 sends
// "x-api-key" here.
func TestHeaderWireNameHTTP2(t *testing.T) {
	assert.Equal(t, "x-api-key", redact.HeaderWireNameHTTP2("X-Api-Key"))
	assert.Equal(t, "authorization", redact.HeaderWireNameHTTP2("Authorization"))
	assert.Equal(t, "x-api-key", redact.HeaderWireNameHTTP2("x-api-key"))
	assert.Equal(t, "naïve", redact.HeaderWireNameHTTP2("naïve"),
		"a name outside printable ASCII is left alone, as LowerHeader leaves it")
}

// TestCookieWireValue covers the one header net/http rewrites rather than
// trims.
//
// Over HTTP/2 the client splits a Cookie value on ";" and sends a field per
// pair, stripping the spaces that followed each separator; the server rejoins
// them with "; ". So a value written without spaces is read back with them.
func TestCookieWireValue(t *testing.T) {
	assert.Equal(t, "session=x; tenant=y", redact.CookieWireValue("session=x;tenant=y"))
	assert.Equal(t, "session=x; tenant=y", redact.CookieWireValue("session=x;   tenant=y"))
	assert.Equal(t, "session=x; tenant=y", redact.CookieWireValue("session=x; tenant=y"))
	assert.Equal(t, "Bearer token", redact.CookieWireValue("Bearer token"),
		"a value with no semicolon is untouched, so this costs nothing elsewhere")
}

// TestBasicAuthHeader pins the credential an endpoint's userinfo turns into.
//
// scheme://user:pass@host works as authentication precisely because
// http.Client derives "Authorization: Basic base64(user:pass)" from it. The
// base64 contains neither half as a substring, so no other rendering in a
// secret list would match a server that echoed the header back.
func TestBasicAuthHeader(t *testing.T) {
	// #nosec G101 -- fabricated fixture endpoint, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const endpoint = "http://alice:s3cret@pyroscope:4040"

	got := redact.BasicAuthHeader(endpoint)

	// Derived the way net/http derives it rather than pasted in, so this
	// fails loudly if the pinned toolchain ever changes the encoding.
	u, err := url.Parse(endpoint)
	require.NoError(t, err)
	password, ok := u.User.Password()
	require.True(t, ok)
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+password))
	assert.Equal(t, want, got)
	assert.NotContains(t, got, "s3cret", "the point: the credential is not a substring of its own header")

	assert.Empty(t, redact.BasicAuthHeader("http://pyroscope:4040"), "no userinfo, no derived header")
	assert.Empty(t, redact.BasicAuthHeader(""))
	assert.Empty(t, redact.BasicAuthHeader("://@"), "an unparseable value yields nothing rather than itself")
}

// TestHeaderValueForms_MatchesWhatAnHTTP2ServerReceives pins the cookie form
// against net/http itself over a real h2 connection, rather than against a
// reading of the encoder.
func TestHeaderValueForms_MatchesWhatAnHTTP2ServerReceives(t *testing.T) {
	// #nosec G101 -- fabricated fixture header value, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const configured = "session=S3cretToken;tenant=acme"

	var received string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		received = r.Header.Get("Cookie")
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	req.Header.Set("Cookie", configured)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, "HTTP/2.0", resp.Proto, "the premise: this went over h2")

	require.NotEqual(t, configured, received, "the premise: the value was rewritten in flight")
	assert.Contains(t, redact.HeaderValueForms(configured), received,
		"the form the server actually received must be in the secret list")
}

// TestInText_FailsClosedOnAJSONEscapedQuerySeparator pins that the closed
// query rule is not defeated by the encoding of its own anchor.
//
// Both rules are anchored on characters, and encoding/json escapes "&" as
// \u0026. So a signed URL quoted inside a Go server's JSON error body reads
// "?tenant=t\u0026Signature=..." and the separator the query rule holds on to
// is simply not there — the rule saw one parameter, "tenant", and passed.
func TestInText_FailsClosedOnAJSONEscapedQuerySeparator(t *testing.T) {
	// #nosec G101 -- fabricated fixture endpoint, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const endpoint = "https://pyroscope:4040/ingest?tenant=t&Signature=S3cretSig"

	body, err := json.Marshal(struct {
		Error string `json:"error"`
	}{Error: "rejected: " + endpoint})
	require.NoError(t, err)
	require.Contains(t, string(body), `\u0026Signature=`,
		"the premise: json escaped the separator, so the raw one is gone")
	require.NotContains(t, string(body), endpoint,
		"and the configured endpoint is no longer a substring to substitute")

	got := redact.InText("upload profile: failed to upload: (401) '"+string(body)+"'", endpoint)

	assert.NotContains(t, got, "S3cretSig")
	assert.Equal(t, "[endpoint redacted]", got)
}

// TestInText_LeavesAnUnrelatedEscapeAlone pins that decoding the text for the
// check does not make the rule fire on messages that carry no credential.
func TestInText_LeavesAnUnrelatedEscapeAlone(t *testing.T) {
	const line = `body: {"error":"bad request: name\u0026id"}`

	assert.Equal(t, line, redact.InText(line))
}

// TestJSONUnescapedIsTotal pins that the decoder used for that second look can
// only ever fail to decode — a malformed escape leaves the text as it stands,
// so the rules see what they would have seen anyway.
func TestJSONUnescapedIsTotal(t *testing.T) {
	for _, line := range []string{
		`no escapes here`,
		`\u`,
		`\uZZZZ`,
		`\u00`,
		`trailing \u002`,
	} {
		t.Run(line, func(t *testing.T) {
			assert.NotPanics(t, func() { _ = redact.InText(line) })
		})
	}
}

// TestInText_FailsClosedOnAnHTMLEscapedQuerySeparator pins the same hole the
// JSON one had, in the other encoding a Go server produces.
//
// html.EscapeString turns "&" into "&amp;", so the query matcher reads the key
// as "amp;Signature" — not a credential key — and both checks pass.
func TestInText_FailsClosedOnAnHTMLEscapedQuerySeparator(t *testing.T) {
	// #nosec G101 -- fabricated fixture endpoint, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const endpoint = "https://pyroscope:4040/ingest?tenant=t&Signature=S3cretSig"

	escaped := html.EscapeString(endpoint)
	require.Contains(t, escaped, "&amp;Signature=", "the premise: the separator is an entity now")
	require.NotContains(t, escaped, endpoint, "so the configured endpoint is not a substring")

	got := redact.InText("body: <p>rejected "+escaped+"</p>", endpoint)

	assert.NotContains(t, got, "S3cretSig")
	assert.Equal(t, "[endpoint redacted]", got)
}

// TestInText_FailsClosedOnAnEscapedAtSign pins the case the review did not
// name, which generalising its finding turned up.
//
// "&commat;" is an at-sign. Both rules therefore see a text with no userinfo
// and nothing to account for — and the substitution runs on the text as it
// stands, so even recognising the decoded form would not let it rewrite this
// one. An anchor an escape hides is an anchor this function cannot reach, so
// the line goes wholesale.
func TestInText_FailsClosedOnAnEscapedAtSign(t *testing.T) {
	// #nosec G101 -- fabricated fixture endpoint, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const escaped = "https://alice:pa55word&commat;pyroscope:4040/ingest"
	require.NotContains(t, escaped, "@", "the premise: there is no at-sign to count")
	require.Contains(t, html.UnescapeString(escaped), "@", "but decoding reveals one")

	got := redact.InText("body: " + escaped)

	assert.NotContains(t, got, "pa55word")
	assert.Equal(t, "[endpoint redacted]", got)
}

// TestInText_LeavesHarmlessEntitiesAlone pins the other side: decoding for the
// check must not start discarding ordinary messages.
func TestInText_LeavesHarmlessEntitiesAlone(t *testing.T) {
	for _, line := range []string{
		`body: <p>bad request: name &amp; id</p>`,
		`body: expected &lt;tag&gt;`,
		`upload profile: failed to upload: (429) 'slow down'`,
	} {
		t.Run(line, func(t *testing.T) {
			assert.Equal(t, line, redact.InText(line))
		})
	}
}

// TestURL_FailsClosedOnAPercentEncodedOpaquePayload pins the same lesson the
// decoded views taught InText, in the one place it had not been applied.
//
// url.URL.String() renders an opaque payload back exactly as it was given, so
// "http:alice%3As3cret%40host" holds neither ":" nor "@" literally and is a
// perfectly reversible "alice:s3cret@host". The rule was written to refuse the
// shapes a "user:pass" pair needs and checked for them as bytes.
func TestURL_FailsClosedOnAPercentEncodedOpaquePayload(t *testing.T) {
	// #nosec G101 -- fabricated fixture URL, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const raw = "http:alice%3As3cret%40host"

	u, err := url.Parse(raw)
	require.NoError(t, err)
	require.NotContains(t, u.Opaque, ":", "the premise: no literal colon")
	require.NotContains(t, u.Opaque, "@", "and no literal at-sign")
	decoded, err := url.PathUnescape(u.Opaque)
	require.NoError(t, err)
	require.Equal(t, "alice:s3cret@host", decoded, "but it decodes to a credential pair")

	assert.Equal(t, "[endpoint redacted]", redact.URL(raw))
	assert.Equal(t, "[endpoint redacted]", redact.URLAttribute(u))
}

// TestURL_StillKeepsAnEncodedHostPortLegible pins the other side: decoding must
// not start refusing opaque payloads that decode to a single token.
func TestURL_StillKeepsAnEncodedHostPortLegible(t *testing.T) {
	for _, raw := range []string{"pyroscope:4040", "collector:4318/v1%2Ftraces"} {
		t.Run(raw, func(t *testing.T) {
			assert.Equal(t, raw, redact.URL(raw))
		})
	}
}

// TestHTMLEscaped covers the rendering a value takes inside an HTML page, the
// fourth form a secret list has to carry.
func TestHTMLEscaped(t *testing.T) {
	assert.Equal(t, "plain", redact.HTMLEscaped("plain"))
	assert.Equal(t, "abc&amp;def", redact.HTMLEscaped("abc&def"))
	assert.Equal(t, "a&lt;b&gt;c", redact.HTMLEscaped("a<b>c"))
	assert.Equal(t, "say &#34;hi&#34;", redact.HTMLEscaped(`say "hi"`))
}

// TestSecrets_RedactsAnHTMLEscapedValue is the end-to-end shape: a credential
// echoed back inside a server's HTML error page.
//
// It is listed as a rendering rather than handled the way InText handles an
// escaped URL because Secrets has to replace inside the text it was given — a
// decoded reading would have nothing to write the replacement back into.
func TestSecrets_RedactsAnHTMLEscapedValue(t *testing.T) {
	// #nosec G101 -- fabricated fixture header value, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const token = "Bearer a&b<c"

	page := "<html><body>rejected Authorization: " + html.EscapeString(token) + "</body></html>"
	require.NotContains(t, page, token, "the premise: the raw form is not in the page")

	got := redact.Secrets(page, redact.Renderings(token)...)

	assert.NotContains(t, got, html.EscapeString(token))
	assert.Contains(t, got, "rejected Authorization:", "the rest of the page survives")
}

// TestInText_FailsClosedOnNestedEntities pins that one decode is a step
// towards a reading of the text, not the reading.
//
// "&amp;amp;Signature" is two passes from "&Signature", and "&amp;commat;" two
// from "@". A single decode left the first still reading as the key
// "amp;Signature" and the second still holding no at-sign, so both rules agreed
// with themselves about a text that was still half encoded.
func TestInText_FailsClosedOnNestedEntities(t *testing.T) {
	// #nosec G101 -- fabricated fixture values, not live credentials
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const nestedQuery = "body: https://pyroscope:4040/ingest?tenant=t&amp;amp;Signature=S3cretSig"
	// #nosec G101 -- fabricated fixture values, not live credentials
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const nestedUserinfo = "body: https://alice:pa55word&amp;commat;pyroscope:4040/ingest"

	require.NotContains(t, html.UnescapeString(nestedQuery), "&Signature",
		"the premise: one decode is not enough for the query case")
	require.NotContains(t, html.UnescapeString(nestedUserinfo), "@",
		"nor for the userinfo one")

	gotQuery := redact.InText(nestedQuery)
	assert.NotContains(t, gotQuery, "S3cretSig")
	assert.Equal(t, "[endpoint redacted]", gotQuery)

	gotUserinfo := redact.InText(nestedUserinfo)
	assert.NotContains(t, gotUserinfo, "pa55word")
	assert.Equal(t, "[endpoint redacted]", gotUserinfo)
}

// TestInText_DoesNotRewriteAnAtSignInAQuery pins that the userinfo pattern
// stops at the query and fragment delimiters.
//
// Without them it read "https://collector?notify=ops@example.com" as userinfo
// running to that at-sign and produced "https://redacted@example.com" — a
// different URL, naming a host that was never contacted. The line is replaced
// wholesale now instead: the at-sign is one the rules cannot account for, and
// saying so is better than quietly rewriting the text into a plausible
// falsehood.
func TestInText_DoesNotRewriteAnAtSignInAQuery(t *testing.T) {
	const clean = "https://collector?notify=ops@example.com"

	got := redact.InText(clean)

	assert.NotContains(t, got, "redacted@example.com",
		"the host must not be rewritten into one that was never contacted")
	assert.Equal(t, "[endpoint redacted]", got)
}

// TestInText_LeavesAFragmentBearingURLAlone pins the same boundary on the
// fragment side, where nothing needs redacting at all.
func TestInText_LeavesAFragmentBearingURLAlone(t *testing.T) {
	const line = "see https://docs.example.com/guide#section-2 for details"

	assert.Equal(t, line, redact.InText(line))
}

// TestURL_FailsClosedOnADoublyEncodedOpaquePayload pins that the opaque rule
// asks about the payload's readings rather than about one step towards them.
//
// "%253A" is an escaped "%3A" is an escaped ":": a payload written that way
// holds neither delimiter after one unescape and both after two, so a single
// pass answered about a string that was still encoded and echoed a reversible
// "alice:secret@host" straight back out.
func TestURL_FailsClosedOnADoublyEncodedOpaquePayload(t *testing.T) {
	// #nosec G101 -- fabricated fixture URL, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const doubled = "http:alice%253Asecret%2540host"

	require.NotContains(t, doubled, ":secret", "the premise: neither delimiter is there literally")
	once, err := url.PathUnescape(doubled)
	require.NoError(t, err)
	require.NotContains(t, once, "@", "nor after the single pass this used to make")
	require.Contains(t, mustUnescapeTwice(t, doubled), "alice:secret@host", "and both are there after the second")

	assert.Equal(t, "[endpoint redacted]", redact.URL(doubled))

	assert.Equal(t, "pyroscope:4040", redact.URL("pyroscope:4040"),
		"a payload that decodes to itself and holds neither delimiter is still legible")
}

// mustUnescapeTwice is the second reading of an opaque payload, for a test that
// asserts what the first one hides.
func mustUnescapeTwice(t *testing.T, raw string) string {
	t.Helper()
	once, err := url.PathUnescape(raw)
	require.NoError(t, err)
	twice, err := url.PathUnescape(once)
	require.NoError(t, err)
	return twice
}

// TestInText_RefusesTextItCouldNotFinishDecoding pins the budget as a reason to
// fail closed rather than a reason to stop looking.
//
// Nested entities are one pass each, so a URL escaped nine times still reads as
// "&amp;Signature" when the budget runs out — an ordinary parameter named
// "amp;Signature" to every rule here, which let the signature through in full.
// The eighth-layer case is asserted beside it, because a bound that refuses
// everything is not a bound.
func TestInText_RefusesTextItCouldNotFinishDecoding(t *testing.T) {
	// #nosec G101 -- fabricated fixture URL, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const signed = "https://h/x?tenant=t&Signature=S3cretSig"

	nested := signed
	for range 9 {
		nested = html.EscapeString(nested)
	}
	require.Contains(t, nested, "S3cretSig", "the premise: the signature is in the text as it stands")

	assert.NotContains(t, redact.InText(nested), "S3cretSig")
	assert.Equal(t, "[endpoint redacted]", redact.InText(nested))

	clean := "dial tcp: lookup collector: no such host"
	for range 8 {
		clean = html.EscapeString(clean)
	}
	assert.Equal(t, clean, redact.InText(clean),
		"a text the decoder does finish is answered about, not refused")
}

// TestInText_DecodesTheEscapesGoWrites pins the third decoder.
//
// GoEscaped is one of the renderings this package says a secret can arrive in,
// so it has to be one of the readings the closed rules are asked about. A "@"
// written as the escape %q uses for an unprintable byte is not an "@" to the
// counting rule, and the userinfo before it went unredacted.
func TestInText_DecodesTheEscapesGoWrites(t *testing.T) {
	// #nosec G101 -- fabricated fixture URL, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const escapedAt = `upload to https://alice:hunter2\x40collector failed`

	require.NotContains(t, escapedAt, "@", "the premise: no at-sign for the closed rule to count")

	assert.Equal(t, "[endpoint redacted]", redact.InText(escapedAt))
}

// TestSecrets_GivesUpTheLineItCannotRewrite pins the closed rule that backs the
// listed renderings up.
//
// Renderings composes the escapings only to renderingDepth, and nothing bounds
// how deeply a server may escape what it echoes. A secret the list cannot match
// literally cannot be replaced — the replacement would have to be written into
// text that does not contain it — so the text goes instead of the value.
func TestSecrets_GivesUpTheLineItCannotRewrite(t *testing.T) {
	// #nosec G101 -- fabricated fixture header value, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const token = "tok&en"

	deep := token
	for range 5 {
		deep = html.EscapeString(deep)
	}
	forms := redact.HeaderValueForms(token)
	require.NotContains(t, forms, deep, "the premise: this depth is past what the list holds")

	assert.Equal(t, "[message redacted]", redact.Secrets("rejected header "+deep, forms...))
}

// TestSecrets_LeavesAMessageWithNoSecretInIt pins the other side of that rule:
// a decoded reading is asked the question the replacement asked, so a message
// that merely contains an escape is not a message that contains a secret.
func TestSecrets_LeavesAMessageWithNoSecretInIt(t *testing.T) {
	// #nosec G101 -- fabricated fixture header value, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const token = "tok&en"

	const message = `{"error":"upstream said \u0026lt;busy\u0026gt; after 13 attempts"}`
	assert.Equal(t, message, redact.Secrets(message, redact.HeaderValueForms(token)...))
	assert.Equal(t, message, redact.Secrets(message, "1"),
		"a short secret keeps the whole-token rule in a decoded reading too: the "+
			`"1" in "13" is not a token in the text, so it is not one in a reading of it`)
}

// TestCredentialHeaderName pins the rule by which a header's value is treated
// as a credential: its name, which is what whoever configured it chose, rather
// than its shape, which this package refuses to guess at anywhere.
func TestCredentialHeaderName(t *testing.T) {
	for _, name := range []string{
		"Authorization", "authorization", "Proxy-Authorization", "Cookie", "Set-Cookie",
		"X-Api-Key", "x-amz-security-token", "X-Goog-Signature", "X-Session-Id",
		"X-Auth-Request-Token", "my-password", "client_secret", "X-Credential",
	} {
		assert.True(t, redact.CredentialHeaderName(name), name)
	}
	for _, name := range []string{
		"", "  ", "Content-Type", "Accept", "User-Agent", "Traceparent", "X-Request-Id",
	} {
		assert.False(t, redact.CredentialHeaderName(name), name)
	}
}

// TestHeaderSecrets covers the values it picks out, the forms it lists them in,
// and the caller-named header a client that lets its token go somewhere else
// needs.
func TestHeaderSecrets(t *testing.T) {
	assert.Nil(t, redact.HeaderSecrets(nil))

	header := http.Header{}
	// #nosec G101 -- fabricated fixture header values, not live credentials
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	header.Set("Authorization", "Bearer tok&en")
	header.Set("Content-Type", "application/json")
	header.Set("X-Tenant-Hdr", "t0ken-in-a-name-no-rule-knows")

	secrets := redact.HeaderSecrets(header)
	assert.Contains(t, secrets, "Bearer tok&en", "the value as configured")
	assert.Contains(t, secrets, "Bearer tok&amp;en", "and as an HTML page holds it")
	assert.NotContains(t, secrets, "application/json", "a header that is not a credential is not one")
	assert.NotContains(t, secrets, "t0ken-in-a-name-no-rule-knows", "nor is a name no rule recognises")

	named := redact.HeaderSecrets(header, "x-tenant-hdr")
	assert.Contains(t, named, "t0ken-in-a-name-no-rule-knows",
		"until the caller says that is where its token goes")

	assert.Equal(t, secrets, redact.HeaderSecrets(header),
		"the order is stable, so a test that pins it cannot flake on map iteration")
}

// TestBasicAuthValue pins that the pair is rendered the way net/http sends it,
// so the same string covers a URL's userinfo and a client's SetBasicAuth.
func TestBasicAuthValue(t *testing.T) {
	// #nosec G101 -- fabricated fixture credential, not a live one
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	value := redact.BasicAuthValue("alice", "hunter2")

	assert.Equal(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("alice:hunter2")), value)
	assert.Equal(t, value, redact.BasicAuthHeader("https://alice:hunter2@collector:4318"),
		"the URL path and the pair path must produce one string, not two")
	assert.NotContains(t, value, "hunter2", "which is why it has to be listed: it holds neither half")
}

// TestInText_DecodesPercentEscapes pins the fourth decoder.
//
// Percent-encoding is not a rendering of a secret but a rendering of a URL, and
// net/url produces it without being asked: an opaque payload keeps its escapes,
// so an error repeating "alice%3Asecret%40host" carries a reversible
// credential with no ":" or "@" for either closed rule to hold on to.
func TestInText_DecodesPercentEscapes(t *testing.T) {
	// #nosec G101 -- fabricated fixture payload, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const escaped = "cannot dial opaque target alice%3Asecret%40host"

	require.NotContains(t, escaped, "@", "the premise: no at-sign for the closed rule to count")
	require.Contains(t, escaped, "secret", "and the credential is in the line as it stands")

	assert.Equal(t, "[endpoint redacted]", redact.InText(escaped))

	const harmless = `Post "https://collector:4318/v1/traces?tenant=a%2Bb": dial tcp: refused`
	assert.Equal(t, harmless, redact.InText(harmless),
		"an escape that decodes to nothing either rule cares about costs the line nothing")
}

// TestError_RefusesACredentialItCannotName pins the redirect case.
//
// net/http derives "Authorization: Basic base64(user:pass)" from a URL's
// userinfo for every hop (client.go:246), so a Location carrying one puts a
// credential on the wire that the request never held. By the time the error
// arrives the password is masked (stripPassword writes "***"), so that header
// cannot be computed here — and a transport that names the header it was given
// has put it in the message. The only thing that can speak for it is the
// caller's own list, and without one the message is given up whole.
func TestError_RefusesACredentialItCannotName(t *testing.T) {
	// #nosec G101 -- fabricated fixture credential, not a live one
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	basic := base64.StdEncoding.EncodeToString([]byte("alice:hunter2"))
	// #nosec G101 -- fabricated fixture URL, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	redirected := &url.Error{
		Op:  "Get",
		URL: "http://alice:***@target/final",
		Err: errors.New(`proxy rejected header "Basic ` + basic + `"`),
	}
	require.Contains(t, redirected.Error(), basic, "the premise: the derived header is in the message")
	require.NotContains(t, redirected.Error(), "hunter2",
		"and the password it was built from is not, so nothing here can compute it")

	assert.Equal(t, "[endpoint redacted]", redact.Error(redirected, nil, nil).Error())

	named := redact.Error(redirected, nil, redact.HeaderValueForms("Basic "+basic)).Error()
	assert.Equal(t, "[endpoint redacted]", named,
		"naming a Basic for that username is not enough on its own: a redirect to "+
			"another host reuses the username and changes the password")

	accounted := redact.Error(
		redirected,
		[]string{"http://alice:hunter2@target"},
		redact.HeaderValueForms("Basic "+basic),
	).Error()
	assert.NotContains(t, accounted, basic, "a caller that named the URL and the credential has it replaced")
	assert.Contains(t, accounted, "target/final", "and keeps the rest of the message")
}

// TestError_RefusesACrossHostRedirectWithTheSameUser pins the hole the
// username-only version of that rule left.
//
// net/http strips the Authorization header on a redirect to another host and
// derives a fresh one from the Location's userinfo, so "alice:oldpass" on the
// request and "alice:newsecret" on the Location produce two different
// credentials with one username. Asking only whether a Basic for "alice" was
// listed answered yes about a credential nothing here had ever seen.
//
// Nothing is lost on a same-host redirect, where net/http copies the original
// header rather than deriving a second one.
func TestError_RefusesACrossHostRedirectWithTheSameUser(t *testing.T) {
	// #nosec G101 -- fabricated fixture credentials, not live ones
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	oldBasic := base64.StdEncoding.EncodeToString([]byte("alice:oldpass"))
	// #nosec G101 -- fabricated fixture credentials, not live ones
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	newBasic := base64.StdEncoding.EncodeToString([]byte("alice:newsecret"))
	require.NotEqual(t, oldBasic, newBasic, "the premise: one username, two credentials")

	// #nosec G101 -- fabricated fixture URL, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	redirected := &url.Error{
		Op:  "Get",
		URL: "http://alice:***@elsewhere/final",
		Err: errors.New(`proxy rejected header "Basic ` + newBasic + `"`),
	}

	got := redact.Error(
		redirected,
		[]string{"http://alice:oldpass@original"},
		redact.HeaderValueForms("Basic "+oldBasic),
	).Error()

	assert.NotContains(t, got, newBasic)
	assert.Equal(t, "[endpoint redacted]", got,
		"the host does not match the endpoint the caller named, so nothing here can speak for it")
}

// TestInText_KeepsARedactedQueryFollowedByAFragment pins that a fragment is not
// part of a query value. Reading through the "#" made this package's own
// output — "Signature=%5Bredacted%5D#section" — look like a signature it had
// never seen, and the whole diagnostic was given up over it.
func TestInText_KeepsARedactedQueryFollowedByAFragment(t *testing.T) {
	const text = `uploading at https://pyroscope:4040/ingest?Signature=%5Bredacted%5D#section name=svc`

	assert.Equal(t, text, redact.InText(text))

	// #nosec G101 -- fabricated fixture URL, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const unredacted = `uploading at https://pyroscope:4040/ingest?Signature=s3cret#section`
	assert.Equal(t, "[endpoint redacted]", redact.InText(unredacted),
		"reading short still fails closed on a value that is not the placeholder")
}

// TestError_ScrubsTheBasicItCanCompute is the other side of that rule: where a
// URL in the chain still carries its password — a custom transport's own error
// carries a URL this SDK never had — the derived header is computed here rather
// than refused, so the message survives with the credential gone.
func TestError_ScrubsTheBasicItCanCompute(t *testing.T) {
	// #nosec G101 -- fabricated fixture credential, not a live one
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	basic := base64.StdEncoding.EncodeToString([]byte("bob:hunter2"))
	// #nosec G101 -- fabricated fixture URL, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	wrapped := &url.Error{
		Op:  "Get",
		URL: "http://bob:hunter2@host/x",
		Err: errors.New(`proxy rejected header "Basic ` + basic + `"`),
	}
	require.Contains(t, wrapped.Error(), basic, "the premise: the derived header is in the message")

	got := redact.Error(wrapped, nil, nil).Error()

	assert.NotContains(t, got, basic, "nothing named it, and it was computed rather than guessed")
	assert.NotContains(t, got, "hunter2")
	assert.Contains(t, got, "host/x", "the rest of the message survives")
}

// TestSecrets_IsUnchangedByRepeatsInItsList pins the property the deduplication
// rests on: dropping a repeat changes what this function does, not what it
// produces.
//
// A caller's list is assembled from several sources that each expand a value
// into every rendering, and the same credential reaches more than one of them.
// The saving is a scan of the text per repeat; the risk of the change is
// dropping something that was not a repeat, which is what this asserts against.
func TestSecrets_IsUnchangedByRepeatsInItsList(t *testing.T) {
	// #nosec G101 -- fabricated fixture credentials, not live ones
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const token, cookie = "Bearer s3cret-token", "sess=c00kie"
	const message = `rejected header "Bearer s3cret-token" and cookie "sess=c00kie" after 1 try`

	distinct := append(redact.HeaderValueForms(token), redact.HeaderValueForms(cookie)...)
	repeated := append(append([]string(nil), distinct...), distinct...)
	require.Greater(t, len(repeated), len(distinct), "the premise: the list holds repeats")

	got := redact.Secrets(message, repeated...)

	assert.Equal(t, redact.Secrets(message, distinct...), got)
	assert.NotContains(t, got, "s3cret-token")
	assert.NotContains(t, got, "c00kie")
	assert.Contains(t, got, "after 1 try", "and nothing else is touched")
}

// TestCookieRequestForms_MatchesWhatNetHTTPSends pins the reimplementation
// against net/http's own AddCookie rather than against a reading of it.
//
// The sanitisation is reimplemented because AddCookie logs to the standard
// logger when it drops a byte, and a redaction must not write anywhere on its
// way to deciding what may be written. A copy of someone else's rule has to be
// checked against the original, so this drives the original and compares.
func TestCookieRequestForms_MatchesWhatNetHTTPSends(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cookie http.Cookie
	}{
		// #nosec G101 -- fabricated fixture cookies, not live credentials
		// nosemgrep: hardcoded-credential-literal,gosec.G101-1
		{"plain", http.Cookie{Name: "session", Value: "c00kie"}},
		{"newline in the value", http.Cookie{Name: "session", Value: "sec\nret"}},
		{"quote and semicolon", http.Cookie{Name: "session", Value: `a"b;c`}},
		{"space", http.Cookie{Name: "session", Value: "a b"}},
		{"comma", http.Cookie{Name: "session", Value: "a,b"}},
		{"backslash", http.Cookie{Name: "session", Value: `a\\b`}},
		{"quoted flag", http.Cookie{Name: "session", Value: "c00kie", Quoted: true}},
		{"newline in the name", http.Cookie{Name: "ses\nsion", Value: "c00kie"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sent := &http.Request{Header: make(http.Header, 1)}
			sent.AddCookie(&tc.cookie)
			pair := sent.Header.Get("Cookie")
			require.NotEmpty(t, pair)

			forms := redact.CookieRequestForms(tc.cookie.Name, tc.cookie.Value, tc.cookie.Quoted)
			assert.Contains(t, forms, pair, "the pair net/http writes")
			_, value, ok := strings.Cut(pair, "=")
			require.True(t, ok)
			assert.Contains(t, forms, value, "and the value on its own")
		})
	}

	assert.Nil(t, redact.CookieRequestForms("session", "\n\r", false),
		"a value that sanitises away is not a secret to list")
}

// TestSecrets_RefusesASecretThatIsThePlaceholder pins the third collision this
// rule has had with its own output.
//
// A secret that is already the string secrets are replaced with cannot be
// replaced: the substitution is a no-op, the decoded readings see no change,
// and the value comes back looking exactly like a redaction that worked —
// indistinguishable from the genuine "[redacted]" beside it in the same line.
func TestSecrets_RefusesASecretThatIsThePlaceholder(t *testing.T) {
	// #nosec G101 -- fabricated fixture credential, not a live one
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const collides = "[redacted]"

	assert.Equal(t, "[message redacted]",
		redact.Secrets("server echoed "+collides+" as the token", collides))

	assert.Equal(t, "[message redacted]",
		redact.Secrets(`{"error":"server echoed &#91;redacted&#93; as the token"}`, collides),
		"a reading of the text carries it just as the text does")

	assert.Equal(t, "no credential here",
		redact.Secrets("no credential here", collides),
		"a line that does not hold it is not given up for it")

	// #nosec G101 -- fabricated fixture credential, not a live one
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const ordinary = "Bearer s3cret-token"
	assert.Equal(t, "server echoed [redacted]",
		redact.Secrets("server echoed "+ordinary, ordinary),
		"and an ordinary secret still produces the placeholder rather than losing the line")
}

// TestError_RefusesACredentialStillInTheText pins the post-condition that
// backs every secret list: after all the replacing, a credential the RFCs
// define is one nothing accounted for, and the text goes.
//
// It exists because two rounds of review found a credential no caller could
// have named — one net/http derived from a redirect's Location, one resty's
// digest transport signed on a copy of the request — and a list cannot be
// extended to cover a value it never sees.
func TestError_RefusesACredentialStillInTheText(t *testing.T) {
	// #nosec G101 -- fabricated fixture credential, not a live one
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	unlisted := base64.StdEncoding.EncodeToString([]byte("alice:newsecret"))
	require.NotContains(t, unlisted, "newsecret", "the premise: base64 hides it from every literal match")

	t.Run("a Basic credential nothing listed", func(t *testing.T) {
		err := errors.New(`proxy rejected header "Basic ` + unlisted + `"`)
		assert.Equal(t, "[message redacted]", redact.Error(err, nil, nil).Error())
	})

	t.Run("one the caller did list is replaced, not refused", func(t *testing.T) {
		err := errors.New(`proxy rejected header "Basic ` + unlisted + `"`)
		got := redact.Error(err, nil, redact.HeaderValueForms("Basic "+unlisted)).Error()
		assert.NotContains(t, got, unlisted)
		assert.Contains(t, got, "proxy rejected header", "the line survives")
	})

	t.Run("a Digest credential, which can never be listed", func(t *testing.T) {
		// #nosec G101 -- fabricated fixture credential, not a live one
		// nosemgrep: hardcoded-credential-literal,gosec.G101-1
		digest := `Digest username="alice", realm="r", nonce="n", uri="/orders", ` +
			`response="41f1670a364ebe0a76bb4740d8b68f45", qop=auth, nc=00000001, cnonce="6ca020a1"`
		err := fmt.Errorf("proxy rejected header %q", digest)
		require.Contains(t, err.Error(), `response=\"41f1`,
			"the premise: %q escapes the quotes, so the rule has to read past the escaping")
		assert.Equal(t, "[message redacted]", redact.Error(err, nil, nil).Error())
	})

	t.Run("prose about authentication is not a credential", func(t *testing.T) {
		for _, message := range []string{
			"Basic authentication failed",
			"server requires Digest or Basic auth",
			"Digest response required but not supplied",
		} {
			err := errors.New(message)
			assert.Equal(t, message, redact.Error(err, nil, nil).Error(), message)
		}
	})
}

// TestHeaderSecrets_ListsTheNameAsWellAsTheValue pins the sibling property the
// profiling and diagnostic lists already had: a credential pasted into the
// name side of a header configuration is still a credential, and net/http
// quotes an unusable name straight back — `invalid header field name "…"`.
func TestHeaderSecrets_ListsTheNameAsWellAsTheValue(t *testing.T) {
	header := http.Header{}
	// #nosec G101 -- fabricated fixture header name, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	pastedAsName := "BearerSecret" + "\n" + "Token"
	header[pastedAsName] = []string{"v"}
	require.True(t, redact.CredentialHeaderName(pastedAsName), "the premise: the name reads as a credential's")

	secrets := redact.HeaderSecrets(header)

	assert.Contains(t, secrets, pastedAsName, "the name as configured")
	assert.Contains(t, secrets, redact.GoEscaped(pastedAsName),
		"and as an error that quotes it renders it")
}
