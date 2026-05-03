// Package mongo provides a tracing-aware MongoDB client wrapper that wires the
// o11y SDK's TracerProvider and Propagator into otel-mongo/v2.
//
// All MongoDB clients in a service should go through this package so command
// spans and optional document trace propagation are configured without relying
// on global OpenTelemetry state.
package mongo

import (
	"context"
	"errors"
	"fmt"

	otelmongo "github.com/Marz32onE/instrumentation-go/otel-mongo/v2"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Option configures MongoDB instrumentation behavior.
type Option func(*config)

type config struct {
	documentTracePropagation bool
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
// tp and prop are required and are passed directly to the upstream wrapper.
// Connect rejects nil values so upstream never falls back to global
// OpenTelemetry providers.
//
// MongoDB command spans are gated by upstream environment variables:
// OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=true and
// OTEL_MONGO_TRACING_ENABLED=true. This wrapper documents those deployment
// requirements but does not set process-global environment variables.
func Connect(
	ctx context.Context,
	uri string,
	tp trace.TracerProvider,
	prop propagation.TextMapPropagator,
	opts ...Option,
) (*otelmongo.Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("mongo connect: context already canceled: %w", err)
	}
	if uri == "" {
		return nil, errors.New("mongo connect: uri must not be empty")
	}
	if tp == nil {
		return nil, errors.New("mongo connect: tracer provider must not be nil")
	}
	if prop == nil {
		return nil, errors.New("mongo connect: propagator must not be nil")
	}

	cfg := newConfig(opts)
	client, err := otelmongo.ConnectWithOptions(
		[]otelmongo.ClientOption{
			otelmongo.WithTracerProvider(tp),
			otelmongo.WithPropagators(prop),
			otelmongo.WithTracePropagationEnabled(cfg.documentTracePropagation),
		},
		options.Client().ApplyURI(uri),
	)
	if err != nil {
		return nil, fmt.Errorf("mongo connect %s: %w", uri, err)
	}
	return client, nil
}
