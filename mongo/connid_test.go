package mongo

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeConnectionID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want string
	}{
		{
			name: "driver format",
			id:   "127.0.0.1:27017[-42]",
			want: "127.0.0.1:27017",
		},
		{
			name: "driver format with resolved hostname",
			id:   "mongo-0.mongo.chat.svc.cluster.local:27017[-27431]",
			want: "mongo-0.mongo.chat.svc.cluster.local:27017",
		},
		{
			name: "driver format with non-default port",
			id:   "mongo:27117[-5]",
			want: "mongo:27117",
		},
		{
			name: "driver format with IPv6 host",
			id:   "[::1]:27017[-7]",
			want: "[::1]:27017",
		},
		{
			name: "driver format with unix socket",
			id:   "/tmp/mongodb-27017.sock[-3]",
			want: "/tmp/mongodb-27017.sock",
		},
		{
			name: "unsigned counter",
			id:   "mongo:27017[42]",
			want: "mongo:27017",
		},
		{
			// The v1 driver and the package's own older fixtures emit a bare
			// address; it must survive untouched.
			name: "no suffix",
			id:   "127.0.0.1:27017",
			want: "127.0.0.1:27017",
		},
		{
			name: "IPv6 host without suffix",
			id:   "[::1]:27017",
			want: "[::1]:27017",
		},
		{
			name: "bare bracketed IPv6 host",
			id:   "[::1]",
			want: "[::1]",
		},
		{
			name: "unix socket without suffix",
			id:   "/tmp/mongodb-27017.sock",
			want: "/tmp/mongodb-27017.sock",
		},
		{
			name: "empty",
			id:   "",
			want: "",
		},
		{
			// An unrecognized decoration is left in place rather than guessed
			// at: dropping it could discard part of a real address.
			name: "non-numeric suffix",
			id:   "mongo:27017[abc]",
			want: "mongo:27017[abc]",
		},
		{
			name: "empty suffix",
			id:   "mongo:27017[]",
			want: "mongo:27017[]",
		},
		{
			name: "sign only suffix",
			id:   "mongo:27017[-]",
			want: "mongo:27017[-]",
		},
		{
			name: "suffix with no address",
			id:   "[-42]",
			want: "[-42]",
		},
		{
			name: "unterminated suffix",
			id:   "mongo:27017[-42",
			want: "mongo:27017[-42",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, normalizeConnectionID(tc.id))
		})
	}
}

func TestIsConnectionCounter(t *testing.T) {
	tests := []struct {
		name string
		s    string
		want bool
	}{
		{name: "signed", s: "-42", want: true},
		{name: "unsigned", s: "42", want: true},
		{name: "zero", s: "0", want: true},
		{name: "empty", s: "", want: false},
		{name: "sign only", s: "-", want: false},
		{name: "letters", s: "abc", want: false},
		{name: "mixed", s: "-4a2", want: false},
		{name: "double sign", s: "--4", want: false},
		{name: "spaced", s: "- 4", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isConnectionCounter(tc.s))
		})
	}
}
