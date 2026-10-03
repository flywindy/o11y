package metrics

import (
	"context"
	"math"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// gatheredTargetInfo gathers reg and returns target_info's labels.
func gatheredTargetInfo(t *testing.T, reg *prometheus.Registry) map[string]string {
	t.Helper()
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, fam := range families {
		if fam.GetName() != "target_info" {
			continue
		}
		require.Len(t, fam.GetMetric(), 1)
		labels := map[string]string{}
		for _, lp := range fam.GetMetric()[0].GetLabel() {
			labels[lp.GetName()] = lp.GetValue()
		}
		return labels
	}
	t.Fatal("no target_info family")
	return nil
}

// TestTargetInfoCollectorMatchesExporter renders target_info for one Resource
// with the SDK's collector and with the pinned otelprom's own, and checks the
// labels agree. The collector exists to keep target_info identical to the
// exporter's, so a bump that changes otelprom's rendering fails here.
func TestTargetInfoCollectorMatchesExporter(t *testing.T) {
	// WithResource merges resource.Environment(); keep the shell out of it.
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
	t.Setenv("OTEL_SERVICE_NAME", "")
	res := resource.NewSchemaless(
		attribute.String("service.name", "svc"),
		attribute.Int("process.pid", 42),
		attribute.BoolSlice("app.flags", []bool{true, false}),
		attribute.StringSlice("app.tags", []string{"a<b", "c&d"}),
		attribute.Float64Slice("app.small", []float64{0.000001}),
		attribute.Float64("app.ratio", math.Inf(1)),
	)

	ours := prometheus.NewRegistry()
	c, err := newTargetInfoCollector(res)
	require.NoError(t, err)
	require.NoError(t, ours.Register(c))

	theirs := prometheus.NewRegistry()
	exp, err := otelprom.New(otelprom.WithRegisterer(theirs))
	require.NoError(t, err)
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exp), sdkmetric.WithResource(res))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	counter, err := mp.Meter("parity").Int64Counter("parity.count")
	require.NoError(t, err)
	counter.Add(context.Background(), 1)

	assert.Equal(t, gatheredTargetInfo(t, theirs), gatheredTargetInfo(t, ours))
}
