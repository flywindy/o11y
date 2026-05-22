package o11y

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseBoolEnv_Absent(t *testing.T) {
	t.Setenv("O11Y_TEST_BOOL", "")
	v, warn := parseBoolEnv("O11Y_TEST_BOOL", true)
	assert.True(t, v, "absent env var should return default")
	assert.Empty(t, warn, "absent env var should produce no warning")
}

func TestParseBoolEnv_ValidTrue(t *testing.T) {
	for _, s := range []string{"1", "t", "T", "true", "TRUE", "True"} {
		t.Setenv("O11Y_TEST_BOOL", s)
		v, warn := parseBoolEnv("O11Y_TEST_BOOL", false)
		assert.True(t, v, "value %q should parse as true", s)
		assert.Empty(t, warn)
	}
}

func TestParseBoolEnv_ValidFalse(t *testing.T) {
	for _, s := range []string{"0", "f", "F", "false", "FALSE", "False"} {
		t.Setenv("O11Y_TEST_BOOL", s)
		v, warn := parseBoolEnv("O11Y_TEST_BOOL", true)
		assert.False(t, v, "value %q should parse as false", s)
		assert.Empty(t, warn)
	}
}

func TestParseBoolEnv_InvalidValue_FallsBackToDefault(t *testing.T) {
	for _, bad := range []string{"yes", "no", "on", "off", "enabled", "2"} {
		t.Setenv("O11Y_TEST_BOOL", bad)
		v, warn := parseBoolEnv("O11Y_TEST_BOOL", true)
		assert.True(t, v, "invalid value %q should fall back to default true", bad)
		assert.NotEmpty(t, warn, "invalid value %q should produce a warning", bad)
		assert.True(t, strings.Contains(warn, "O11Y_TEST_BOOL"),
			"warning should name the env var; got: %s", warn)
		assert.True(t, strings.Contains(warn, bad),
			"warning should echo the bad value; got: %s", warn)
	}
}

func TestWithExtraHTTPServerAttributeKeys_AccumulatesAndSkipsEmpty(t *testing.T) {
	cfg := &Config{}
	WithExtraHTTPServerAttributeKeys("app_name", "")(cfg)
	WithExtraHTTPServerAttributeKeys("bot_name")(cfg)
	WithExtraHTTPServerAttributeKeys()(cfg)
	assert.Equal(t, []string{"app_name", "bot_name"}, cfg.extraHTTPServerAttrKeys)
	assert.Empty(t, cfg.initWarnings, "valid keys should not emit warnings")
}

func TestWithExtraHTTPServerAttributeKeys_RejectsReservedCollisions(t *testing.T) {
	// Both dot and underscore forms normalize to the same Prometheus label
	// name; accepting them would silently merge two attribute values into
	// the same exported label and corrupt PromQL grouping.
	colliders := []struct {
		input    string
		expected string
	}{
		{"http_route", "http_route"},
		{"http.route", "http_route"},
		{"http_request_method", "http_request_method"},
		{"http.request.method", "http_request_method"},
		{"http_response_status_code", "http_response_status_code"},
		{"service.name", "service_name"},
		{"service_namespace", "service_namespace"},
		{"deployment.environment.name", "deployment_environment_name"},
	}
	for _, c := range colliders {
		cfg := &Config{}
		WithExtraHTTPServerAttributeKeys(c.input, "app_name")(cfg)
		assert.Equal(t, []string{"app_name"}, cfg.extraHTTPServerAttrKeys,
			"collider %q must be dropped, only non-colliding key %q kept", c.input, "app_name")
		assert.Lenf(t, cfg.initWarnings, 1,
			"collider %q must emit exactly one warning", c.input)
		if len(cfg.initWarnings) == 1 {
			assert.Contains(t, cfg.initWarnings[0], c.input,
				"warning must echo the rejected input key")
			assert.Contains(t, cfg.initWarnings[0], c.expected,
				"warning must name the collided Prometheus label")
		}
	}
}
