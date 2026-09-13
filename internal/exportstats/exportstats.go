// Package exportstats counts OTLP export failures per signal and exposes the
// count as an SDK-owned instrument, so "the collector was unreachable" has a
// number attached instead of a line on stderr.
//
// The OTel SDK's batchers (BatchSpanProcessor, the log BatchProcessor and the
// metric PeriodicReader) hand each failed batch to otel.Handle and move on;
// the spans, records or data points in that batch are gone. Nothing in the
// SDK counts them. The wrappers here sit between the batcher and the OTLP
// exporter, count every batch the exporter rejects, and leave the error
// untouched so the batcher's own reporting still runs.
package exportstats

import (
	"context"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

// Signal names the pipeline a failure belongs to. Each maps to the semconv
// otel.component.type of the OTLP/HTTP exporter that serves it, which is the
// attribute the counter carries (see ComponentType).
type Signal string

// The three OTLP pipelines the SDK exports on.
const (
	SignalTraces  Signal = "traces"
	SignalLogs    Signal = "logs"
	SignalMetrics Signal = "metrics"
)

// InstrumentName is the OTel name of the failure counter. otelprom renders it
// as o11y_export_failures_total; the unit is a braces annotation and adds no
// suffix. The name is SDK-owned: semconv v1.39.0's exporter self-metrics
// (otel.sdk.exporter.span.exported and siblings) count exported items with
// error.type on failure, are at status Development, and are the names the
// OTel Go SDK will emit itself once it ships self-observability; a
// package-local name keeps this batch-level failure counter from colliding
// with those when they land. See docs/semconv.md.
const InstrumentName = "o11y.export.failures"

// ScopeName is the instrumentation scope the SDK registers its own
// instruments under. otelprom renders it as the otel_scope_name label.
const ScopeName = "github.com/flywindy/o11y"

// ComponentType returns the semconv otel.component.type attribute for the
// exporter that serves signal (otelprom renders the key as the
// otel_component_type label), or an empty KeyValue for an unknown signal.
func ComponentType(signal Signal) attribute.KeyValue {
	switch signal {
	case SignalTraces:
		return semconv.OTelComponentTypeOtlpHTTPSpanExporter
	case SignalLogs:
		return semconv.OTelComponentTypeOtlpHTTPLogExporter
	case SignalMetrics:
		return semconv.OTelComponentTypeOtlpHTTPMetricExporter
	default:
		return attribute.KeyValue{}
	}
}

// Recorder holds one failure count per signal. The zero value is ready to
// use; Fail is safe for concurrent use and never blocks, so it can run inside
// an exporter's Export path.
type Recorder struct {
	traces  atomic.Int64
	logs    atomic.Int64
	metrics atomic.Int64
}

// Fail records one failed export batch for signal. Unknown signals are
// ignored rather than counted under a wrong label.
func (r *Recorder) Fail(signal Signal) {
	if c := r.counter(signal); c != nil {
		c.Add(1)
	}
}

// Failures returns the number of failed batches recorded for signal.
func (r *Recorder) Failures(signal Signal) int64 {
	if c := r.counter(signal); c != nil {
		return c.Load()
	}
	return 0
}

// counter returns the atomic behind signal, or nil for an unknown signal.
func (r *Recorder) counter(signal Signal) *atomic.Int64 {
	switch signal {
	case SignalTraces:
		return &r.traces
	case SignalLogs:
		return &r.logs
	case SignalMetrics:
		return &r.metrics
	default:
		return nil
	}
}

// Register creates the o11y.export.failures observable counter on meter,
// reporting one data point per signal, attributed by otel.component.type,
// from the Recorder's counts. It is
// observable rather than synchronous so the counts can start accumulating
// before the MeterProvider exists (the tracer is built first) and so the
// metric pipeline's own failures can be counted without re-entering it.
func (r *Recorder) Register(meter metric.Meter) error {
	_, err := meter.Int64ObservableCounter(InstrumentName,
		metric.WithDescription("Export batches the OTLP exporters failed to deliver; the spans, log records or data points in a failed batch are dropped."),
		metric.WithUnit("{batch}"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			for _, signal := range []Signal{SignalTraces, SignalLogs, SignalMetrics} {
				o.Observe(r.Failures(signal), metric.WithAttributes(ComponentType(signal)))
			}
			return nil
		}),
	)
	return err
}

// SpanExporter wraps inner so every failed ExportSpans call is counted
// under SignalTraces. Shutdown and the error itself pass through unchanged.
func SpanExporter(inner sdktrace.SpanExporter, r *Recorder) sdktrace.SpanExporter {
	return spanExporter{SpanExporter: inner, recorder: r}
}

type spanExporter struct {
	sdktrace.SpanExporter
	recorder *Recorder
}

// ExportSpans implements sdktrace.SpanExporter.
func (e spanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	err := e.SpanExporter.ExportSpans(ctx, spans)
	if err != nil {
		e.recorder.Fail(SignalTraces)
	}
	return err
}

// LogExporter wraps inner so every failed Export call is counted under
// SignalLogs. Shutdown, ForceFlush and the error itself pass through
// unchanged.
func LogExporter(inner sdklog.Exporter, r *Recorder) sdklog.Exporter {
	return logExporter{Exporter: inner, recorder: r}
}

type logExporter struct {
	sdklog.Exporter
	recorder *Recorder
}

// Export implements sdklog.Exporter.
func (e logExporter) Export(ctx context.Context, records []sdklog.Record) error {
	err := e.Exporter.Export(ctx, records)
	if err != nil {
		e.recorder.Fail(SignalLogs)
	}
	return err
}

// MetricExporter wraps inner so every failed Export call is counted under
// SignalMetrics. Temporality, Aggregation, ForceFlush and Shutdown pass
// through unchanged. A count taken on the OTLP metrics path is only visible
// once an export succeeds again; it still answers "how many collections were
// lost while the collector was down" after the fact.
func MetricExporter(inner sdkmetric.Exporter, r *Recorder) sdkmetric.Exporter {
	return metricExporter{Exporter: inner, recorder: r}
}

type metricExporter struct {
	sdkmetric.Exporter
	recorder *Recorder
}

// Export implements sdkmetric.Exporter.
func (e metricExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	err := e.Exporter.Export(ctx, rm)
	if err != nil {
		e.recorder.Fail(SignalMetrics)
	}
	return err
}
