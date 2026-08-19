package redact_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

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

// TestURLNeverLeaksThePassword is the property that matters: whatever shape the
// endpoint takes, the secret must not survive into the logged string.
func TestURLNeverLeaksThePassword(t *testing.T) {
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
	const override = "http://user:s3cret%zz@adhoc-host:4040"
	text := `parse "` + override + `": invalid URL escape "%zz"`

	got := redact.InText(text, configured)

	assert.NotContains(t, got, "s3cret",
		"an endpoint the SDK never configured must still have its credential scrubbed")
	assert.Contains(t, got, "adhoc-host", "the host stays, so the failure is still diagnosable")
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
		{"an email address is not a URL credential", "notify ops@example.com on failure", "", "notify ops@example.com on failure"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, redact.InText(tt.text, tt.endpoint))
		})
	}
}
