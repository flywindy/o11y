package exportstats_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"

	"github.com/flywindy/o11y/internal/exportstats"
)

var errUpstream = errors.New("dial tcp 10.0.0.5:4318: connect: connection refused")

// flakySpanExporter fails every call while fail is set.
type flakySpanExporter struct {
	sdktrace.SpanExporter
	fail bool
}

// ExportSpans implements sdktrace.SpanExporter.
func (f *flakySpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if f.fail {
		return errUpstream
	}
	return f.SpanExporter.ExportSpans(ctx, spans)
}

// flakyLogExporter fails every call while fail is set.
type flakyLogExporter struct {
	sdklog.Exporter
	fail bool
}

// Export implements sdklog.Exporter.
func (f *flakyLogExporter) Export(context.Context, []sdklog.Record) error {
	if f.fail {
		return errUpstream
	}
	return nil
}

// flakyMetricExporter fails every call while fail is set.
type flakyMetricExporter struct {
	sdkmetric.Exporter
	fail bool
}

// Export implements sdkmetric.Exporter.
func (f *flakyMetricExporter) Export(context.Context, *metricdata.ResourceMetrics) error {
	if f.fail {
		return errUpstream
	}
	return nil
}

// TestWrappers_CountOnlyFailedBatchesAndPassErrorsThrough drives each wrapper
// through a failing and a healthy call and checks only the failures count.
func TestWrappers_CountOnlyFailedBatchesAndPassErrorsThrough(t *testing.T) {
	var rec exportstats.Recorder
	ctx := context.Background()

	spanInner := &flakySpanExporter{SpanExporter: tracetest.NewInMemoryExporter(), fail: true}
	spans := exportstats.SpanExporter(spanInner, &rec)
	err := spans.ExportSpans(ctx, nil)
	require.ErrorIs(t, err, errUpstream, "the batcher must still see the exporter's error")
	spanInner.fail = false
	require.NoError(t, spans.ExportSpans(ctx, nil))

	logInner := &flakyLogExporter{fail: true}
	logs := exportstats.LogExporter(logInner, &rec)
	require.ErrorIs(t, logs.Export(ctx, nil), errUpstream)
	require.ErrorIs(t, logs.Export(ctx, nil), errUpstream)
	logInner.fail = false
	require.NoError(t, logs.Export(ctx, nil))

	metricInner := &flakyMetricExporter{fail: true}
	metrics := exportstats.MetricExporter(metricInner, &rec)
	require.ErrorIs(t, metrics.Export(ctx, &metricdata.ResourceMetrics{}), errUpstream)
	metricInner.fail = false
	require.NoError(t, metrics.Export(ctx, &metricdata.ResourceMetrics{}))

	assert.Equal(t, int64(1), rec.Failures(exportstats.SignalTraces))
	assert.Equal(t, int64(2), rec.Failures(exportstats.SignalLogs))
	assert.Equal(t, int64(1), rec.Failures(exportstats.SignalMetrics))
	assert.Equal(t, int64(0), rec.Failures(exportstats.Signal("unknown")))
}

// TestRecorder_UnknownSignalIsIgnored pins that a stray signal is dropped
// rather than counted under a wrong label.
func TestRecorder_UnknownSignalIsIgnored(t *testing.T) {
	var rec exportstats.Recorder
	rec.Fail(exportstats.Signal("profiles"))
	for _, s := range []exportstats.Signal{exportstats.SignalTraces, exportstats.SignalLogs, exportstats.SignalMetrics} {
		assert.Equal(t, int64(0), rec.Failures(s))
	}
}

// TestRegister_ObservesOnePointPerConstructedExporter collects the
// observable counter through a manual reader and checks it reports every
// signal that has an exporter wrapped through the Recorder, including one
// with no failures (a zero rather than no series), and nothing for a signal
// without one: on the Prometheus pull path there is no OTLP metric exporter,
// and a zero there would advertise a component that does not exist.
func TestRegister_ObservesOnePointPerConstructedExporter(t *testing.T) {
	var rec exportstats.Recorder
	_ = exportstats.SpanExporter(&flakySpanExporter{}, &rec)
	_ = exportstats.LogExporter(&flakyLogExporter{}, &rec)
	rec.Fail(exportstats.SignalTraces)
	rec.Fail(exportstats.SignalTraces)
	rec.Fail(exportstats.SignalMetrics) // counted, but never observed: no exporter
	assert.True(t, rec.Active(exportstats.SignalTraces))
	assert.True(t, rec.Active(exportstats.SignalLogs))
	assert.False(t, rec.Active(exportstats.SignalMetrics))
	assert.False(t, rec.Active(exportstats.Signal("profiles")))

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	require.NoError(t, rec.Register(provider.Meter("test")))

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	require.Len(t, rm.ScopeMetrics, 1)
	require.Len(t, rm.ScopeMetrics[0].Metrics, 1)
	m := rm.ScopeMetrics[0].Metrics[0]
	assert.Equal(t, exportstats.InstrumentName, m.Name)
	assert.Equal(t, "{batch}", m.Unit)

	sum, ok := m.Data.(metricdata.Sum[int64])
	require.True(t, ok, "expected an int64 sum, got %T", m.Data)
	assert.True(t, sum.IsMonotonic)
	assert.Equal(t, metricdata.CumulativeTemporality, sum.Temporality)

	got := map[attribute.Value]int64{}
	for _, dp := range sum.DataPoints {
		require.Equal(t, 1, dp.Attributes.Len(), "exactly one attribute per data point")
		v, ok := dp.Attributes.Value(semconv.OTelComponentTypeKey)
		require.True(t, ok, "every data point carries the semconv otel.component.type attribute")
		got[v] = dp.Value
	}
	assert.Equal(t, map[attribute.Value]int64{
		semconv.OTelComponentTypeOtlpHTTPSpanExporter.Value: 2,
		semconv.OTelComponentTypeOtlpHTTPLogExporter.Value:  0,
	}, got, "the log exporter reports a zero, the absent metric exporter reports nothing")
}

// TestRegister_NoExportersNoSeries checks a Recorder nothing was wrapped
// through contributes no data points, so the instrument is absent rather
// than three zeros.
func TestRegister_NoExportersNoSeries(t *testing.T) {
	var rec exportstats.Recorder
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	require.NoError(t, rec.Register(provider.Meter("test")))

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok {
				assert.Empty(t, sum.DataPoints)
			}
		}
	}
}

// TestComponentType pins the signal to semconv component-type mapping and the
// empty value for an unknown signal.
func TestComponentType(t *testing.T) {
	assert.Equal(t, semconv.OTelComponentTypeOtlpHTTPSpanExporter, exportstats.ComponentType(exportstats.SignalTraces))
	assert.Equal(t, semconv.OTelComponentTypeOtlpHTTPLogExporter, exportstats.ComponentType(exportstats.SignalLogs))
	assert.Equal(t, semconv.OTelComponentTypeOtlpHTTPMetricExporter, exportstats.ComponentType(exportstats.SignalMetrics))
	assert.False(t, exportstats.ComponentType(exportstats.Signal("profiles")).Valid())
}
