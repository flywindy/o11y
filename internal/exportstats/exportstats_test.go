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

	"github.com/flywindy/o11y/internal/exportstats"
)

var errUpstream = errors.New("dial tcp 10.0.0.5:4318: connect: connection refused")

// flakySpanExporter fails every call while fail is set.
type flakySpanExporter struct {
	sdktrace.SpanExporter
	fail bool
}

func (f *flakySpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if f.fail {
		return errUpstream
	}
	return f.SpanExporter.ExportSpans(ctx, spans)
}

type flakyLogExporter struct {
	sdklog.Exporter
	fail bool
}

func (f *flakyLogExporter) Export(context.Context, []sdklog.Record) error {
	if f.fail {
		return errUpstream
	}
	return nil
}

type flakyMetricExporter struct {
	sdkmetric.Exporter
	fail bool
}

func (f *flakyMetricExporter) Export(context.Context, *metricdata.ResourceMetrics) error {
	if f.fail {
		return errUpstream
	}
	return nil
}

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

func TestRecorder_UnknownSignalIsIgnored(t *testing.T) {
	var rec exportstats.Recorder
	rec.Fail(exportstats.Signal("profiles"))
	for _, s := range []exportstats.Signal{exportstats.SignalTraces, exportstats.SignalLogs, exportstats.SignalMetrics} {
		assert.Equal(t, int64(0), rec.Failures(s))
	}
}

// TestRegister_ObservesOnePointPerSignal collects the observable counter
// through a manual reader and checks it reports every signal, including the
// ones with no failures, so a dashboard sees a zero rather than no series.
func TestRegister_ObservesOnePointPerSignal(t *testing.T) {
	var rec exportstats.Recorder
	rec.Fail(exportstats.SignalTraces)
	rec.Fail(exportstats.SignalTraces)
	rec.Fail(exportstats.SignalLogs)

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

	got := map[string]int64{}
	for _, dp := range sum.DataPoints {
		v, ok := dp.Attributes.Value(attribute.Key("signal"))
		require.True(t, ok, "every data point carries the signal attribute")
		got[v.AsString()] = dp.Value
	}
	assert.Equal(t, map[string]int64{"traces": 2, "logs": 1, "metrics": 0}, got)
}
