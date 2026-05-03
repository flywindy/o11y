package mongo

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func newTestProviders() (oteltrace.TracerProvider, propagation.TextMapPropagator, *tracetest.SpanRecorder) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sr),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})
	return tp, prop, sr
}

func enableMongoTracing(t *testing.T) {
	t.Helper()
	t.Setenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", "true")
	t.Setenv("OTEL_MONGO_TRACING_ENABLED", "true")
}

func TestWithDocumentTracePropagation(t *testing.T) {
	cases := []struct {
		name string
		opt  Option
		want bool
	}{
		{"default off", nil, false},
		{"explicit on", WithDocumentTracePropagation(true), true},
		{"explicit off", WithDocumentTracePropagation(false), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var opts []Option
			if tc.opt != nil {
				opts = append(opts, tc.opt)
			}

			cfg := newConfig(opts)
			assert.Equal(t, tc.want, cfg.documentTracePropagation)
		})
	}
}

func TestConnect_Validation(t *testing.T) {
	tp, prop, _ := newTestProviders()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name    string
		ctx     context.Context
		uri     string
		tp      oteltrace.TracerProvider
		prop    propagation.TextMapPropagator
		wantErr string
	}{
		{"canceled ctx", canceled, "mongodb://localhost:27017", tp, prop, context.Canceled.Error()},
		{"empty uri", context.Background(), "", tp, prop, "uri must not be empty"},
		{"nil tracer provider", context.Background(), "mongodb://localhost:27017", nil, prop, "tracer provider must not be nil"},
		{"nil propagator", context.Background(), "mongodb://localhost:27017", tp, nil, "propagator must not be nil"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, err := Connect(tc.ctx, tc.uri, tc.tp, tc.prop)
			require.Error(t, err)
			assert.Nil(t, client)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestConnect_InvalidURI(t *testing.T) {
	tp, prop, _ := newTestProviders()

	client, err := Connect(context.Background(), "mongodb://:invalid", tp, prop)
	require.Error(t, err)
	assert.Nil(t, client)
}

func TestConnect_DefaultTracingGateEmitsNoSpans(t *testing.T) {
	tp, prop, sr := newTestProviders()

	client, err := Connect(context.Background(), "mongodb://127.0.0.1:1", tp, prop)
	require.NoError(t, err)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = client.Disconnect(shutdownCtx)
	}()

	opCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _ = client.Database("o11y_test").Collection("events").InsertOne(opCtx, bson.M{"name": "default-gate"})

	assert.Empty(t, sr.Ended(), "upstream env gates should keep MongoDB tracing disabled by default")
}

func TestConnect_DocumentPropagationCannotBypassMasterGate(t *testing.T) {
	t.Setenv("OTEL_MONGO_PROPAGATION_ENABLED", "true")

	tp, prop, sr := newTestProviders()

	client, err := Connect(
		context.Background(),
		"mongodb://127.0.0.1:1",
		tp,
		prop,
		WithDocumentTracePropagation(true),
	)
	require.NoError(t, err)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = client.Disconnect(shutdownCtx)
	}()

	opCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _ = client.Database("o11y_test").Collection("events").InsertOne(opCtx, bson.M{"name": "document-gate"})

	assert.Empty(t, sr.Ended(), "document propagation opt-in must not bypass the upstream master tracing gate")
}

func TestConnect_UsesProvidedTracerProvider(t *testing.T) {
	enableMongoTracing(t)

	tp, prop, sr := newTestProviders()

	client, err := Connect(context.Background(), "mongodb://127.0.0.1:1", tp, prop)
	require.NoError(t, err)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = client.Disconnect(shutdownCtx)
	}()

	opCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _ = client.Database("o11y_test").Collection("events").InsertOne(opCtx, bson.M{"name": "provided-tracer"})

	require.NotEmpty(t, sr.Ended(), "MongoDB operation should emit a span through the supplied provider")
	attrs := sr.Ended()[0].Attributes()
	assert.Contains(t, attrs, attribute.String("db.system.name", "mongodb"))
	assert.Contains(t, attrs, attribute.String("db.namespace", "o11y_test"))
	assert.Contains(t, attrs, attribute.String("db.collection.name", "events"))
	assert.Contains(t, attrs, attribute.String("db.operation.name", "insert"))
	assert.NotContains(t, attrs, attribute.String("db.system", "mongodb"))
}
