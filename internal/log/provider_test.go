package log

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLogEndpointURL verifies that logEndpointURL appends /v1/logs when no
// path is given and preserves explicit paths supplied by the caller.
func TestLogEndpointURL(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "bare host and port",
			input: "http://localhost:4318",
			want:  "http://localhost:4318/v1/logs",
		},
		{
			name:  "trailing slash only",
			input: "http://localhost:4318/",
			want:  "http://localhost:4318/v1/logs",
		},
		{
			name:  "explicit default path",
			input: "http://localhost:4318/v1/logs",
			want:  "http://localhost:4318/v1/logs",
		},
		{
			name:  "custom path without trailing slash",
			input: "http://collector.example.com:4318/custom/logs",
			want:  "http://collector.example.com:4318/custom/logs",
		},
		{
			name:  "custom path with trailing slash is trimmed",
			input: "http://collector.example.com:4318/custom/logs/",
			want:  "http://collector.example.com:4318/custom/logs",
		},
		{
			name:    "invalid URL",
			input:   "://bad-url",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := logEndpointURL(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
