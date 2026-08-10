package o11y

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/flywindy/o11y/internal/baggageattrs"
	o11ylog "github.com/flywindy/o11y/internal/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
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

func TestWithExemplars_OverridesDefault(t *testing.T) {
	// Default config carries exemplars=true so trace-to-metric linkage works
	// out of the box; WithExemplars(false) is the documented opt-out for
	// services migrating dashboards that hardcode integer le boundaries.
	cfg := defaultConfig()
	assert.True(t, cfg.exemplars, "default must be true")
	WithExemplars(false)(cfg)
	assert.False(t, cfg.exemplars, "WithExemplars(false) must flip the flag")
	WithExemplars(true)(cfg)
	assert.True(t, cfg.exemplars, "WithExemplars(true) must restore the flag")
}

func TestWithUserBaggage_EnablesUserBaggageMaterialization(t *testing.T) {
	cfg := defaultConfig()
	assert.False(t, cfg.userBaggage, "user baggage materialization must default off")

	WithUserBaggage()(cfg)

	assert.True(t, cfg.userBaggage, "WithUserBaggage must enable the opt-in flag")
}

func TestWithBaggageAttributesAccumulatesDeduplicatesAndWarns(t *testing.T) {
	cfg := defaultConfig()
	cfg.initWarnings = nil

	WithBaggageAttributes("app.order.id", "", "service.name", "user.name")(cfg)
	WithBaggageAttributes("app.order.id", "app.site.id")(cfg)

	assert.Equal(t, []string{"app.order.id", "app.site.id"}, cfg.baggageKeys)
	require.Len(t, cfg.initWarnings, 3)
	assert.Contains(t, strings.Join(cfg.initWarnings, "\n"), "must not be empty")
	assert.Contains(t, strings.Join(cfg.initWarnings, "\n"), "service.name")
	assert.Contains(t, strings.Join(cfg.initWarnings, "\n"), "WithUserBaggage")
}

func TestWithBaggageAttributesRejectsEverySDKLogField(t *testing.T) {
	reserved := []string{
		"service.name", "service.version", "service.namespace", "environment",
		"deployment.environment.name", "traceId", "spanId", "time", "level",
		"msg", "source", "http.response.status_code", "http.response.body.size",
		"process.pid", "http.request.header.authorization",
	}
	cfg := defaultConfig()
	cfg.initWarnings = nil
	WithBaggageAttributes(append(reserved, "app.order.id")...)(cfg)

	assert.Equal(t, []string{"app.order.id"}, cfg.baggageKeys)
	require.Len(t, cfg.initWarnings, len(reserved))
	for _, key := range reserved {
		assert.Contains(t, strings.Join(cfg.initWarnings, "\n"), key)
	}
}

func TestWithBaggageAttributesWarnsOncePerInvalidKey(t *testing.T) {
	cfg := defaultConfig()
	cfg.initWarnings = nil

	WithBaggageAttributes("msg", "msg", "app.order.id", "msg")(cfg)

	assert.Equal(t, []string{"app.order.id"}, cfg.baggageKeys)
	require.Len(t, cfg.initWarnings, 1, "a repeated invalid key must not warn once per occurrence")
	assert.Contains(t, cfg.initWarnings[0], `dropping key "msg"`)
}

func TestConfigureBaggageWhitelistAppliesCapAfterCollisionCheck(t *testing.T) {
	cfg := defaultConfig()
	cfg.initWarnings = nil
	keys := make([]string, MaxBaggageAttributeKeys+1)
	for i := range keys {
		keys[i] = fmt.Sprintf("app.key.%d", i)
	}
	WithBaggageAttributes(keys...)(cfg)
	res := resource.NewSchemaless(attribute.String(keys[len(keys)-1], "resource-value"))

	_, err := configureBaggageWhitelist(cfg, res)
	overCap := keys[len(keys)-1]
	require.EqualError(t, err, "baggage attribute keys collide with resource attributes: "+overCap+
		" ("+overCap+" exceed MaxBaggageAttributeKeys="+strconv.Itoa(MaxBaggageAttributeKeys)+
		" and would not have been materialized; the collision check runs before the cap"+
		" so its result does not depend on key order)")
	assert.Empty(t, cfg.initWarnings, "collision must fail before the overflow warning/truncation")
}

func TestConfigureBaggageWhitelistCollisionWithinCapOmitsOverflowNote(t *testing.T) {
	cfg := defaultConfig()
	cfg.initWarnings = nil
	WithBaggageAttributes("app.order.id", "app.tenant.id")(cfg)
	res := resource.NewSchemaless(attribute.String("app.order.id", "resource-value"))

	_, err := configureBaggageWhitelist(cfg, res)
	require.EqualError(t, err, "baggage attribute keys collide with resource attributes: app.order.id")
}

func TestConfigureBaggageWhitelistKeepsUserOutsideApplicationCap(t *testing.T) {
	for _, userFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("user-first=%v", userFirst), func(t *testing.T) {
			cfg := defaultConfig()
			cfg.initWarnings = nil
			keys := make([]string, MaxBaggageAttributeKeys+1)
			for i := range keys {
				keys[i] = fmt.Sprintf("app.key.%d", i)
			}
			if userFirst {
				WithUserBaggage()(cfg)
			}
			WithBaggageAttributes(keys...)(cfg)
			if !userFirst {
				WithUserBaggage()(cfg)
			}

			whitelist, err := configureBaggageWhitelist(cfg, resource.Empty())
			require.NoError(t, err)
			assert.Equal(t, MaxBaggageAttributeKeys+1, whitelist.Len())
			assert.Equal(t, baggageattrs.UserNameKey, whitelist.Keys()[0])
			require.Len(t, cfg.initWarnings, 1)
			assert.Contains(t, cfg.initWarnings[0], "first 8")
		})
	}
}

func TestConfigureBaggageWhitelistReportsSortedResourceCollisions(t *testing.T) {
	cfg := defaultConfig()
	WithUserBaggage()(cfg)
	WithBaggageAttributes("app.z", "app.a")(cfg)
	res := resource.NewSchemaless(
		attribute.String("app.z", "z"),
		attribute.String(baggageattrs.UserNameKey, "user"),
		attribute.String("app.a", "a"),
	)

	_, err := configureBaggageWhitelist(cfg, res)
	require.EqualError(t, err, "baggage attribute keys collide with resource attributes: app.a, app.z, user.name")
}

func TestInitRejectsBaggageCollisionFromOTELResourceAttributes(t *testing.T) {
	base := []Option{
		WithServiceName("test"), WithServiceVersion("1.0.0"),
		WithServiceNamespace("test"), WithEnvironment("testing"),
		WithTraceEnabled(false), WithMetricsEnabled(false), WithLogEnabled(false),
	}
	for _, tt := range []struct {
		name string
		env  string
		opt  Option
		key  string
	}{
		{"application", "app.order.id=resource", WithBaggageAttributes("app.order.id"), "app.order.id"},
		{"user", "user.name=resource", WithUserBaggage(), "user.name"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OTEL_RESOURCE_ATTRIBUTES", tt.env)
			obs, err := Init(context.Background(), append(base, tt.opt)...)
			require.Error(t, err)
			assert.Nil(t, obs)
			assert.Contains(t, err.Error(), tt.key)
		})
	}
}

func TestInitInstallsBaggageHandlerOnlyForEffectiveWhitelist(t *testing.T) {
	base := []Option{
		WithServiceName("test"), WithServiceVersion("1.0.0"),
		WithServiceNamespace("test"), WithEnvironment("testing"),
		WithTraceEnabled(false), WithMetricsEnabled(false), WithLogEnabled(false),
	}

	without, err := Init(context.Background(), base...)
	require.NoError(t, err)
	defer func() { _ = without.Shutdown(context.Background()) }()
	_, installed := without.Logger.Handler().(*o11ylog.BaggageHandler)
	assert.False(t, installed)

	withUser, err := Init(context.Background(), append(base, WithBaggageAttributes(), WithUserBaggage())...)
	require.NoError(t, err)
	defer func() { _ = withUser.Shutdown(context.Background()) }()
	_, installed = withUser.Logger.Handler().(*o11ylog.BaggageHandler)
	assert.True(t, installed, "user.name alone makes the effective whitelist non-empty")
}

func TestAppendBaggageWarnings_WhenTraceDisabled(t *testing.T) {
	cfg := defaultConfig()
	cfg.initWarnings = nil
	cfg.traceEnabled = false
	WithUserBaggage()(cfg)

	appendBaggageWarnings(cfg, baggageattrs.NewWhitelist(baggageattrs.UserNameKey))

	require.Len(t, cfg.initWarnings, 1)
	assert.Contains(t, cfg.initWarnings[0], "trace pillar disabled")
	assert.Contains(t, cfg.initWarnings[0], "not spans")
}

func TestAppendBaggageWarnings_WhenTraceEnabled(t *testing.T) {
	cfg := defaultConfig()
	cfg.initWarnings = nil
	WithUserBaggage()(cfg)

	appendBaggageWarnings(cfg, baggageattrs.NewWhitelist(baggageattrs.UserNameKey))

	assert.Empty(t, cfg.initWarnings)
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

func TestWithMaxUniqueCollections_DefaultAndClamp(t *testing.T) {
	// The Cassandra table label ships on (ADR 0019 §7, 2026-07-29 amendment), so
	// a default cap must be installed without the caller opting in — otherwise a
	// mis-parsed statement shape would grow db.collection.name without bound.
	cfg := defaultConfig()
	assert.Equal(t, DefaultMaxUniqueCollections, cfg.maxUniqueCollections,
		"the collection cap must be active by default")

	WithMaxUniqueCollections(25)(cfg)
	assert.Equal(t, 25, cfg.maxUniqueCollections)

	// Non-positive values restore the default rather than disabling the cap,
	// matching WithMaxUniqueRoutes: an accidental 0 must not silently uncap a
	// label that is emitted by default.
	WithMaxUniqueCollections(0)(cfg)
	assert.Equal(t, DefaultMaxUniqueCollections, cfg.maxUniqueCollections)
	WithMaxUniqueCollections(-1)(cfg)
	assert.Equal(t, DefaultMaxUniqueCollections, cfg.maxUniqueCollections)
}

// The two caps are independent knobs; setting one must not disturb the other.
func TestWithMaxUniqueCollections_IndependentOfRouteCap(t *testing.T) {
	cfg := defaultConfig()
	WithMaxUniqueCollections(25)(cfg)
	assert.Equal(t, DefaultMaxUniqueRoutes, cfg.maxUniqueRoutes)
	WithMaxUniqueRoutes(7)(cfg)
	assert.Equal(t, 25, cfg.maxUniqueCollections)
}
