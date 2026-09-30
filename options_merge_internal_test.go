package o11y

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestWithProfilingAuthHeaders_Merges pins the documented merge semantics:
// repeated calls merge into one map, a later call overwrites an earlier value
// for the same key, an empty map changes nothing, and the option copies the
// caller's map so a later mutation of it does not leak in.
func TestWithProfilingAuthHeaders_Merges(t *testing.T) {
	first := map[string]string{"Authorization": "Basic a", "X-Scope-OrgID": "tenant-1"}
	cfg := &Config{}
	for _, opt := range []Option{
		WithProfilingAuthHeaders(first),
		WithProfilingAuthHeaders(nil),
		WithProfilingAuthHeaders(map[string]string{}),
		WithProfilingAuthHeaders(map[string]string{"X-Scope-OrgID": "tenant-2", "X-Extra": "e"}),
	} {
		opt(cfg)
	}
	first["Authorization"] = "mutated after the option was applied"

	assert.Equal(t, map[string]string{
		"Authorization": "Basic a",
		"X-Scope-OrgID": "tenant-2",
		"X-Extra":       "e",
	}, cfg.profilingAuthHeaders)
}
