package redact_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/flywindy/o11y/internal/redact"
)

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
		{"unparseable but contains an @", "http://u:p@host:4040/\x7f\x00", "[unparseable endpoint redacted]"},
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
	} {
		assert.NotContains(t, redact.URL(raw), secret, "input %q leaked its credential", raw)
	}
}
