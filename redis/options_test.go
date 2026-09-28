package redis

import (
	"context"
	"testing"

	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

// TestWithCommandTextEnabled drives the option through Wrap: db.query.text is
// recorded only when it is enabled, is absent by default, and the last call
// wins when the option is given more than once. The client points at an
// address nothing listens on; the hook records the span either way.
func TestWithCommandTextEnabled(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []Option
		want bool
	}{
		{name: "default off", want: false},
		{name: "enabled", opts: []Option{WithCommandTextEnabled(true)}, want: true},
		{name: "disabled", opts: []Option{WithCommandTextEnabled(false)}, want: false},
		{name: "last call wins", opts: []Option{WithCommandTextEnabled(true), WithCommandTextEnabled(false)}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp, sr, mp, _ := newRedisTestProviders()
			client := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
			defer client.Close()

			_, err := Wrap(client, tp, mp, append([]Option{WithPoolName("command-text")}, tc.opts...)...)
			require.NoError(t, err)
			defer Unwrap(client)

			_ = client.Process(context.Background(), goredis.NewCmd(context.Background(), "set", "session:42", "secret-value"))

			spans := sr.Ended()
			require.Len(t, spans, 1)
			value, ok := spanAttr(spans[0], semconv.DBQueryTextKey)
			assert.Equal(t, tc.want, ok, "db.query.text present")
			if tc.want {
				assert.Equal(t, "set session:42 secret-value", value.AsString())
			}
		})
	}
}
