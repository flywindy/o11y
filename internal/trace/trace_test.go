package trace

import (
	"context"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/flywindy/o11y/internal/exportstats"
	"github.com/flywindy/o11y/internal/testutil"
)

func testResource() *resource.Resource {
	return resource.NewSchemaless(semconv.ServiceName("trace-test"))
}

// TestInitTracer_ExportsWithHeaders checks spans reach the endpoint's
// /v1/traces with the configured headers, the failure recorder is activated
// for traces and counts nothing on success, and the caller's extra span
// processors see the span (a nil one is skipped).
func TestInitTracer_ExportsWithHeaders(t *testing.T) {
	srv := testutil.NewCapturingOTLPServer(t)
	failures := &exportstats.Recorder{}
	extra := tracetest.NewSpanRecorder()

	tp, _, err := InitTracer(context.Background(), srv.URL, map[string]string{"Authorization": "Bearer t"},
		testResource(), sdktrace.AlwaysSample(), failures, nil, extra)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	_, span := tp.Tracer("test").Start(context.Background(), "op")
	span.End()
	require.NoError(t, tp.ForceFlush(context.Background()))

	requests := srv.Requests()
	require.NotEmpty(t, requests)
	assert.Equal(t, "/v1/traces", requests[0].Path)
	assert.Equal(t, "Bearer t", requests[0].Header.Get("Authorization"))
	assert.True(t, failures.Active(exportstats.SignalTraces))
	assert.Zero(t, failures.Failures(exportstats.SignalTraces))
	require.Len(t, extra.Ended(), 1, "extra span processors receive the span")
	assert.Equal(t, "op", extra.Ended()[0].Name())
}

// TestInitTracer_URLPath pins where spans are sent: a bare endpoint goes to
// the OTLP default /v1/traces, and an explicit path, the root included, is
// used as given.
func TestInitTracer_URLPath(t *testing.T) {
	for _, tc := range []struct {
		name, suffix, want string
	}{
		{"bare endpoint", "", "/v1/traces"},
		{"root path", "/", "/"},
		{"custom path", "/otlp/traces", "/otlp/traces"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := testutil.NewCapturingOTLPServer(t)
			tp, _, err := InitTracer(context.Background(), srv.URL+tc.suffix, nil, testResource(), sdktrace.AlwaysSample(), nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

			_, span := tp.Tracer("test").Start(context.Background(), "op")
			span.End()
			require.NoError(t, tp.ForceFlush(context.Background()))

			requests := srv.Requests()
			require.NotEmpty(t, requests)
			assert.Equal(t, tc.want, requests[0].Path)
		})
	}
}

// TestInitTracer_IgnoresProtocolEnv checks spans stay protobuf-encoded when
// the environment asks for http/json, which the trace exporter honours since
// v1.46 but the log and metric exporters do not.
func TestInitTracer_IgnoresProtocolEnv(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/json")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "http/json")
	srv := testutil.NewCapturingOTLPServer(t)
	tp, _, err := InitTracer(context.Background(), srv.URL, nil, testResource(), sdktrace.AlwaysSample(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	_, span := tp.Tracer("test").Start(context.Background(), "op")
	span.End()
	require.NoError(t, tp.ForceFlush(context.Background()))

	requests := srv.Requests()
	require.NotEmpty(t, requests)
	assert.Equal(t, "application/x-protobuf", requests[0].Header.Get("Content-Type"))
}

// TestInitTracer_CountsExportFailures points the exporter at a server that
// rejects everything (400 is not retried) and checks the failure is counted
// under traces.
func TestInitTracer_CountsExportFailures(t *testing.T) {
	srv := testutil.NewCapturingOTLPServerWithStatus(t, http.StatusBadRequest)
	failures := &exportstats.Recorder{}

	tp, _, err := InitTracer(context.Background(), srv.URL, nil, testResource(), sdktrace.AlwaysSample(), failures)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	_, span := tp.Tracer("test").Start(context.Background(), "op")
	span.End()
	_ = tp.ForceFlush(context.Background())

	assert.Equal(t, int64(1), failures.Failures(exportstats.SignalTraces))
}

// TestInitTracer_Sampler checks a given sampler is installed and that nil
// keeps the SDK default, which records a root span. With a nil sampler the SDK
// reads OTEL_TRACES_SAMPLER, so the test unsets it: an empty value still counts
// as set and is reported as an unsupported sampler. t.Setenv first, so the
// original value is restored afterwards.
func TestInitTracer_Sampler(t *testing.T) {
	for _, key := range []string{"OTEL_TRACES_SAMPLER", "OTEL_TRACES_SAMPLER_ARG"} {
		t.Setenv(key, "")
		require.NoError(t, os.Unsetenv(key))
	}
	srv := testutil.NewCapturingOTLPServer(t)
	for _, tc := range []struct {
		name    string
		sampler sdktrace.Sampler
		want    bool
	}{
		{name: "nil keeps the default", sampler: nil, want: true},
		{name: "never sample", sampler: sdktrace.NeverSample(), want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp, _, err := InitTracer(context.Background(), srv.URL, nil, testResource(), tc.sampler, nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

			_, span := tp.Tracer("test").Start(context.Background(), "op")
			defer span.End()
			assert.Equal(t, tc.want, span.SpanContext().IsSampled())
		})
	}
}

// TestInitTracer_Propagator checks the returned propagator carries both W3C
// trace context and baggage.
func TestInitTracer_Propagator(t *testing.T) {
	srv := testutil.NewCapturingOTLPServer(t)
	tp, prop, err := InitTracer(context.Background(), srv.URL, nil, testResource(), nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	member, err := baggage.NewMember("tenant", "acme")
	require.NoError(t, err)
	bag, err := baggage.New(member)
	require.NoError(t, err)
	ctx, span := tp.Tracer("test").Start(baggage.ContextWithBaggage(context.Background(), bag), "op")
	defer span.End()

	carrier := propagation.MapCarrier{}
	prop.Inject(ctx, carrier)
	assert.NotEmpty(t, carrier.Get("traceparent"))
	assert.Equal(t, "tenant=acme", carrier.Get("baggage"))

	extracted := prop.Extract(context.Background(), carrier)
	assert.Equal(t, span.SpanContext().TraceID(), oteltrace.SpanContextFromContext(extracted).TraceID())
	assert.Equal(t, "acme", baggage.FromContext(extracted).Member("tenant").Value())
}
