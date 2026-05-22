package o11y

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	// The otelprom exporter rewrites every non-alphanumeric rune to '_' and
	// collapses runs, so callers can shadow built-in SDK labels via dots,
	// dashes, slashes, or repeated underscores. The option must catch all
	// of those before they reach the exporter.
	colliders := []struct {
		input    string
		expected string
	}{
		{"http_route", "http_route"},
		{"http.route", "http_route"},
		{"http-route", "http_route"},
		{"http/route", "http_route"},
		{"http__route", "http_route"},
		{"HTTP.ROUTE", "HTTP_ROUTE"}, // sanitizer is case-preserving
		{"http_request_method", "http_request_method"},
		{"http.request.method", "http_request_method"},
		{"http-request-method", "http_request_method"},
		{"http_response_status_code", "http_response_status_code"},
		{"service.name", "service_name"},
		{"service_namespace", "service_namespace"},
		{"service-version", "service_version"},
		{"deployment.environment.name", "deployment_environment_name"},
		{"otel.scope.name", "otel_scope_name"},
		{"otel_scope_version", "otel_scope_version"},
		{"otel.scope.schema_url", "otel_scope_schema_url"},
	}
	for _, c := range colliders {
		cfg := &Config{}
		WithExtraHTTPServerAttributeKeys(c.input, "app_name")(cfg)
		// HTTP.ROUTE normalizes to HTTP_ROUTE which is NOT in the reserved
		// set (which only contains lowercase forms), so it should be
		// accepted. Skip it for the rejection assertion.
		if c.expected == "HTTP_ROUTE" {
			assert.Contains(t, cfg.extraHTTPServerAttrKeys, c.input,
				"uppercase variant %q does not collide with lowercase built-ins", c.input)
			continue
		}
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

func TestWithExtraHTTPServerAttributeKeys_RejectsCallerCollisions(t *testing.T) {
	// Two caller-supplied keys can normalize to the same Prometheus label
	// even when neither matches a built-in. The exporter would merge their
	// values into a single label value (semicolon-joined), making the
	// dimension ambiguous in PromQL. Detection must work both within a
	// single call and across calls.
	t.Run("within_single_call", func(t *testing.T) {
		cfg := &Config{}
		WithExtraHTTPServerAttributeKeys("app.name", "app_name", "bot_name")(cfg)
		assert.Equal(t, []string{"app.name", "bot_name"}, cfg.extraHTTPServerAttrKeys,
			"first occurrence wins; second collider is rejected")
		require.Len(t, cfg.initWarnings, 1)
		assert.Contains(t, cfg.initWarnings[0], `"app_name"`)
		assert.Contains(t, cfg.initWarnings[0], `app.name`,
			"warning should name the prior key that occupies the normalized label")
		assert.Contains(t, cfg.initWarnings[0], `app_name`,
			"warning should name the conflicting normalized label")
	})
	t.Run("across_calls", func(t *testing.T) {
		cfg := &Config{}
		WithExtraHTTPServerAttributeKeys("app.name")(cfg)
		WithExtraHTTPServerAttributeKeys("app-name", "bot_name")(cfg)
		assert.Equal(t, []string{"app.name", "bot_name"}, cfg.extraHTTPServerAttrKeys)
		require.Len(t, cfg.initWarnings, 1)
		assert.Contains(t, cfg.initWarnings[0], `"app-name"`)
	})
}

func TestWithExtraHTTPServerAttributeKeys_RejectsInvalidNormalization(t *testing.T) {
	cfg := &Config{}
	// All non-alphanumerics → entire key normalizes to a single underscore,
	// which is not a usable Prometheus label name.
	WithExtraHTTPServerAttributeKeys("...", "app_name")(cfg)
	assert.Equal(t, []string{"app_name"}, cfg.extraHTTPServerAttrKeys)
	require.Len(t, cfg.initWarnings, 1)
	assert.Contains(t, cfg.initWarnings[0], "normalizes to an invalid")
}

func TestNormalizePrometheusLabelName(t *testing.T) {
	cases := map[string]string{
		"":                "",
		"http.route":      "http_route",
		"http-route":      "http_route",
		"http/route":      "http_route",
		"http__route":     "http_route",
		"http...route":    "http_route",
		"app.name":        "app_name",
		"app_name":        "app_name",
		"123app":          "key_123app",
		"1.2.3":           "key_1_2_3",
		"...":             "_",
		"_already_safe_":  "_already_safe_",
		"HTTPRoute":       "HTTPRoute",
		"my service.name": "my_service_name",
	}
	for in, want := range cases {
		assert.Equalf(t, want, normalizePrometheusLabelName(in),
			"normalizePrometheusLabelName(%q)", in)
	}
}
