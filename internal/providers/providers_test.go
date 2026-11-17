package providers_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/flywindy/o11y/internal/providers"
)

func TestOrNoop(t *testing.T) {
	realTP := sdktrace.NewTracerProvider()
	realMP := sdkmetric.NewMeterProvider()
	realProp := propagation.TraceContext{}

	tests := []struct {
		name     string
		tp       trace.TracerProvider
		mp       metric.MeterProvider
		prop     propagation.TextMapPropagator
		wantTP   trace.TracerProvider
		wantMP   metric.MeterProvider
		wantProp propagation.TextMapPropagator
	}{
		{
			name:     "all nil become implementations that do nothing",
			wantTP:   tracenoop.NewTracerProvider(),
			wantMP:   metricnoop.NewMeterProvider(),
			wantProp: propagation.NewCompositeTextMapPropagator(),
		},
		{
			name: "values that were given are returned as they are",
			tp:   realTP, mp: realMP, prop: realProp,
			wantTP: realTP, wantMP: realMP, wantProp: realProp,
		},
		{
			name:     "each one is decided on its own",
			tp:       realTP,
			wantTP:   realTP,
			wantMP:   metricnoop.NewMeterProvider(),
			wantProp: propagation.NewCompositeTextMapPropagator(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tp, mp, prop := providers.OrNoop(tt.tp, tt.mp, tt.prop)

			assert.Equal(t, tt.wantTP, tp)
			assert.Equal(t, tt.wantMP, mp)
			assert.Equal(t, tt.wantProp, prop)
		})
	}
}

// TestOrNoop_NilPropagatorWritesAndReadsNothing pins what "does nothing" means
// for the propagator: no header on the way out, no span context on the way in.
func TestOrNoop_NilPropagatorWritesAndReadsNothing(t *testing.T) {
	_, _, prop := providers.OrNoop(nil, nil, nil)
	// Compared as an interface value, which is how the upstream libraries test
	// it. The empty composite is a nil slice underneath, so a reflective
	// nil check (require.NotNil) would call it nil when no caller sees it so.
	require.True(t, prop != nil, "an upstream `if prop != nil` must see a propagator")

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1},
		SpanID:     trace.SpanID{2},
		TraceFlags: trace.FlagsSampled,
	})
	header := http.Header{}
	prop.Inject(trace.ContextWithSpanContext(context.Background(), sc), propagation.HeaderCarrier(header))
	assert.Empty(t, header, "nothing is injected")

	header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	ctx := prop.Extract(context.Background(), propagation.HeaderCarrier(header))
	assert.False(t, trace.SpanContextFromContext(ctx).IsValid(), "and nothing is extracted")
}
