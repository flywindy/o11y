// Package mongo provides a tracing- and metrics-aware MongoDB client wrapper
// that wires the o11y SDK's TracerProvider, MeterProvider, and Propagator into
// MongoDB instrumentation.
//
// All MongoDB clients in a service should go through this package so command
// spans and optional document trace propagation are configured without relying
// on global OpenTelemetry state.
package mongo

import (
	"context"
	"errors"
	"fmt"

	marzmongo "github.com/Marz32onE/instrumentation-go/otel-mongo/v2"
	"go.mongodb.org/mongo-driver/v2/event"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	otelmongo "go.opentelemetry.io/contrib/instrumentation/go.mongodb.org/mongo-driver/v2/mongo/otelmongo"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// Option configures MongoDB instrumentation behavior.
type Option func(*config)

type config struct {
	documentTracePropagation bool
}

// Client is a tracing-aware MongoDB client.
//
// It embeds the upstream otel-mongo/v2 client so driver methods remain
// available while application code can name the type without importing the
// upstream instrumentation package directly.
type Client struct {
	*marzmongo.Client
}

// WithDocumentTracePropagation controls whether the upstream MongoDB wrapper
// injects W3C trace context into persisted documents under the "_oteltrace"
// field.
//
// The default is false because enabling this changes document shape, may
// require schema validation changes, and stores high-cardinality trace context
// in MongoDB. Services should enable it only for asynchronous patterns such as
// change streams, outbox processors, or delayed jobs that need to restore trace
// context from a document.
func WithDocumentTracePropagation(enabled bool) Option {
	return func(cfg *config) {
		cfg.documentTracePropagation = enabled
	}
}

func newConfig(opts []Option) config {
	cfg := config{}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return cfg
}

// Connect establishes an instrumented MongoDB client.
//
// The returned client is backed by otel-mongo/v2 and supports the official
// go.mongodb.org/mongo-driver/v2 API through embedded MongoDB driver types.
// ctx is checked before client construction; the underlying driver does not
// dial until the first operation or Ping.
//
// tp, mp, and prop are required. The tracer provider and propagator are passed
// directly to the upstream wrapper. The meter provider is wired into the
// official OTel contrib CommandMonitor in metrics-only mode. Connect rejects
// nil values so upstream never falls back to global OpenTelemetry providers.
//
// MongoDB command spans are gated by upstream environment variables:
// OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=true and
// OTEL_MONGO_TRACING_ENABLED=true. This wrapper documents those deployment
// requirements but does not set process-global environment variables.
func Connect(
	ctx context.Context,
	uri string,
	tp trace.TracerProvider,
	mp metric.MeterProvider,
	prop propagation.TextMapPropagator,
	opts ...Option,
) (*Client, error) {
	if ctx == nil {
		return nil, errors.New("mongo connect: context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("mongo connect: context already canceled: %w", err)
	}
	if uri == "" {
		return nil, errors.New("mongo connect: uri must not be empty")
	}
	if tp == nil {
		return nil, errors.New("mongo connect: tracer provider must not be nil")
	}
	if mp == nil {
		return nil, errors.New("mongo connect: meter provider must not be nil")
	}
	if prop == nil {
		return nil, errors.New("mongo connect: propagator must not be nil")
	}

	cfg := newConfig(opts)
	clientOptions := options.Client().ApplyURI(uri)
	clientOptions.SetMonitor(newMetricsMonitor(mp))

	client, err := marzmongo.ConnectWithOptions(
		[]marzmongo.ClientOption{
			marzmongo.WithTracerProvider(tp),
			marzmongo.WithPropagators(prop),
			marzmongo.WithTracePropagationEnabled(cfg.documentTracePropagation),
		},
		clientOptions,
	)
	if err != nil {
		return nil, fmt.Errorf("mongo connect: %w", err)
	}
	return &Client{Client: client}, nil
}

func newMetricsMonitor(mp metric.MeterProvider) *event.CommandMonitor {
	return otelmongo.NewMonitor(
		otelmongo.WithMeterProvider(mp),
		otelmongo.WithTracerProvider(tracenoop.NewTracerProvider()),
		otelmongo.WithCommandAttributeDisabled(true),
	)
}
