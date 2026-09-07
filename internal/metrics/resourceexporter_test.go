package metrics

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
)

// capturingExporter records the Resource of every batch it is handed.
type capturingExporter struct {
	mu        sync.Mutex
	resources []*resource.Resource
}

func (c *capturingExporter) Temporality(k sdkmetric.InstrumentKind) metricdata.Temporality {
	return sdkmetric.DefaultTemporalitySelector(k)
}

func (c *capturingExporter) Aggregation(k sdkmetric.InstrumentKind) sdkmetric.Aggregation {
	return sdkmetric.DefaultAggregationSelector(k)
}

func (c *capturingExporter) Export(_ context.Context, rm *metricdata.ResourceMetrics) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resources = append(c.resources, rm.Resource)
	return nil
}

func (c *capturingExporter) ForceFlush(context.Context) error { return nil }
func (c *capturingExporter) Shutdown(context.Context) error   { return nil }

// TestGuardedResourceExporter_ReplacesProviderResource drives a real
// MeterProvider: sdkmetric.WithResource merges the environment back in, so
// the provider's Resource carries the alias the guard rejected; the wrapped
// exporter must still ship the guarded Resource.
func TestGuardedResourceExporter_ReplacesProviderResource(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "telemetry_sdk_name=evil")

	guarded := resource.NewSchemaless(
		attribute.String("service.name", "svc"),
		attribute.String("telemetry.sdk.name", "opentelemetry"),
	)
	captured := &capturingExporter{}
	reader := sdkmetric.NewPeriodicReader(withGuardedResource(captured, guarded))
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithResource(guarded))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	counter, err := provider.Meter("test").Int64Counter("hits")
	require.NoError(t, err)
	counter.Add(t.Context(), 1)
	require.NoError(t, provider.ForceFlush(t.Context()))

	require.NotEmpty(t, captured.resources)
	exported := captured.resources[len(captured.resources)-1]
	assert.Same(t, guarded, exported, "the exporter must ship the guarded Resource, not the provider's")
	_, hasAlias := exported.Set().Value("telemetry_sdk_name")
	assert.False(t, hasAlias, "the environment alias must not be exported")
	v, _ := exported.Set().Value("telemetry.sdk.name")
	assert.Equal(t, "opentelemetry", v.AsString())
}
