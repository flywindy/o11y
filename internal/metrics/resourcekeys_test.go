package metrics

import (
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
)

func TestResourceKeyOwner(t *testing.T) {
	cases := []struct {
		key      attribute.Key
		owner    attribute.Key
		identity bool
	}{
		{"service.name", "service.name", true},
		{"service_name", "service.name", true},
		{"service-name", "service.name", true},
		{"deployment.environment.name", "deployment.environment.name", true},
		{"telemetry.sdk.name", "telemetry.sdk.name", false},
		{"telemetry_sdk_version", "telemetry.sdk.version", false},
		{"process_pid", "process.pid", false},
		{"host-name", "host.name", false},
		{"Service.Name", "", false}, // normalization is case-preserving: a distinct label
		{"app.shard", "", false},
		{"telemetry.sdk.extra", "", false},
	}
	for _, tc := range cases {
		owner, identity := ResourceKeyOwner(tc.key)
		assert.Equal(t, tc.owner, owner, string(tc.key))
		assert.Equal(t, tc.identity, identity, string(tc.key))
	}
	assert.True(t, IsTelemetrySDKKey("telemetry.sdk.extra"))
	assert.True(t, IsTelemetrySDKKey("telemetry_sdk_name"))
	assert.False(t, IsTelemetrySDKKey("telemetry.other"))
	assert.True(t, IsProcessKey("process.command_args"))
	assert.True(t, IsProcessKey("process_owner"))
	assert.True(t, IsProcessKey("process.pid"))
	assert.False(t, IsProcessKey("processor.count"))
}

// warningFor returns the single warning that quotes key, failing the test
// when there is none or more than one.
func warningFor(t *testing.T, warnings []string, key string) string {
	t.Helper()
	var matches []string
	for _, w := range warnings {
		if strings.Contains(w, strconv.Quote(key)) {
			matches = append(matches, w)
		}
	}
	require.Len(t, matches, 1, "expected one warning for %q, got %v", key, warnings)
	return matches[0]
}

func TestEnvResourceAttributes(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", strings.Join([]string{
		"service.name=exact",      // exact identity key: kept, the SDK overrides it later
		"service_name=alias",      // alias of an identity key: dropped
		"telemetry.sdk.name=mine", // exact detected key: kept, the detector overrides it later
		"telemetry_sdk_name=evil", // alias of a detected key: dropped
		"telemetry.sdk.extra=x",   // SDK namespace: dropped
		"process.pid=7",           // exact detected key: kept, the detector overrides it later
		"process.command_args=x",  // process.* namespace: dropped, the detectors leave it out on purpose
		"__meta__=x",              // untranslatable label: dropped
		"app_foo=env",             // alias of a caller key: dropped
		"app.bar=env",             // same key as a caller key: kept, the merge overrides it
		"env.x=1",                 // kept
		"env_x=2",                 // alias of env.x, which sorts first: dropped
	}, ","))
	t.Setenv("OTEL_SERVICE_NAME", "from-env") // overrides service.name from the list, still the exact key

	kept, dropped, warnings := EnvResourceAttributes(t.Context(), []attribute.KeyValue{
		attribute.String("app.foo", "code"),
		attribute.String("app.bar", "code"),
	})

	assert.ElementsMatch(t, []attribute.KeyValue{
		attribute.String("service.name", "from-env"),
		attribute.String("telemetry.sdk.name", "mine"),
		attribute.String("process.pid", "7"),
		attribute.String("app.bar", "env"),
		attribute.String("env.x", "1"),
	}, kept)

	assert.ElementsMatch(t, []attribute.Key{
		"service_name", "telemetry_sdk_name", "telemetry.sdk.extra", "process.command_args", "__meta__", "app_foo", "env_x",
	}, dropped, "every dropped key is reported so the provider Resource can neutralize it")
	neutralized := NeutralizeEnvKeys(dropped)
	require.Len(t, neutralized, len(dropped))
	for i, kv := range neutralized {
		assert.Equal(t, dropped[i], kv.Key)
		assert.Equal(t, "", kv.Value.AsString())
	}
	assert.Nil(t, NeutralizeEnvKeys(nil))

	require.Len(t, warnings, 7)
	assert.Contains(t, warningFor(t, warnings, "process.command_args"), "deliberately not exported")
	assert.Contains(t, warningFor(t, warnings, "service_name"), `"service.name"`)
	assert.Contains(t, warningFor(t, warnings, "telemetry_sdk_name"), `"telemetry.sdk.name"`)
	assert.Contains(t, warningFor(t, warnings, "telemetry.sdk.extra"), "telemetry.sdk.*")
	assert.Contains(t, warningFor(t, warnings, "__meta__"), "target_info")
	assert.Contains(t, warningFor(t, warnings, "app_foo"), `"app.foo"`)
	assert.Contains(t, warningFor(t, warnings, "env_x"), `"env.x"`)
	for _, w := range warnings {
		assert.True(t, strings.HasPrefix(w, "OTEL_RESOURCE_ATTRIBUTES:"), w)
	}
}

func TestEnvResourceAttributes_Unset(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
	t.Setenv("OTEL_SERVICE_NAME", "")
	kept, dropped, warnings := EnvResourceAttributes(t.Context(), nil)
	assert.Empty(t, kept)
	assert.Empty(t, dropped)
	assert.Empty(t, warnings)
}

// TestTargetInfoCollector renders target_info from a Resource the way the
// Prometheus path does and checks the label rules: otelprom-compatible names,
// untranslatable keys skipped, first key wins on a label collision.
func TestTargetInfoCollector(t *testing.T) {
	res := resource.NewSchemaless(
		attribute.String("service.name", "svc"),
		attribute.String("telemetry.sdk.name", "opentelemetry"),
		attribute.String("app.x", "first"),
		attribute.String("app_x", "second"), // same label as app.x; app.x sorts first
		attribute.String("__meta__", "dropped"),
		attribute.String("...", "dropped"),
		attribute.Int("process.pid", 42),
		attribute.BoolSlice("app.flags", []bool{true, false}), // otelprom renders slices with Emit
		attribute.Float64("app.ratio", math.Inf(1)),           // and non-finite floats as +Inf
	)
	c, err := newTargetInfoCollector(res)
	require.NoError(t, err)

	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(c))
	families, err := reg.Gather()
	require.NoError(t, err)
	require.Len(t, families, 1)
	fam := families[0]
	assert.Equal(t, "target_info", fam.GetName())
	assert.Equal(t, "Target metadata", fam.GetHelp())
	require.Len(t, fam.GetMetric(), 1)
	assert.Equal(t, float64(1), fam.GetMetric()[0].GetGauge().GetValue())

	labels := map[string]string{}
	for _, lp := range fam.GetMetric()[0].GetLabel() {
		labels[lp.GetName()] = lp.GetValue()
	}
	assert.Equal(t, map[string]string{
		"service_name":       "svc",
		"telemetry_sdk_name": "opentelemetry",
		"app_x":              "first",
		"process_pid":        "42",
		"app_flags":          "[true false]",
		"app_ratio":          "+Inf",
	}, labels)
}
