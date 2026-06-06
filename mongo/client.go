// Package mongo wires the o11y SDK's TracerProvider and MeterProvider into the
// official OpenTelemetry MongoDB driver instrumentation without relying on
// global OpenTelemetry state.
package mongo

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/event"
	drivermongo "go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	otelmongo "go.opentelemetry.io/contrib/instrumentation/go.mongodb.org/mongo-driver/v2/mongo/otelmongo"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// Connect establishes an instrumented MongoDB client.
//
// The returned client is the plain go.mongodb.org/mongo-driver/v2 *mongo.Client
// so application repositories can keep using standard driver types. ctx is
// checked before client construction; the underlying driver does not dial until
// the first operation or Ping.
//
// tp, mp, and prop are required. The tracer and meter providers are wired into
// the official OTel contrib CommandMonitor with command spans and
// db.client.operation.duration enabled. Command spans are always-on and governed
// by the supplied TracerProvider's sampler; this package does not read or
// reproduce the old Marz OTEL_*_ENABLED gates.
func Connect(
	ctx context.Context,
	uri string,
	tp trace.TracerProvider,
	mp metric.MeterProvider,
	prop propagation.TextMapPropagator,
) (*drivermongo.Client, error) {
	if ctx == nil {
		return nil, errors.New("mongo connect: context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("mongo connect: context already canceled: %w", err)
	}
	if uri == "" {
		return nil, errors.New("mongo connect: uri must not be empty")
	}

	clientOptions := options.Client().ApplyURI(uri)
	// TODO(ADR 0014 Phase 2): if Instrument starts registering pool-metric
	// observables, surface that cleanup lifecycle from Connect instead of
	// leaving it available only to application-built client options.
	if _, err := Instrument(clientOptions, tp, mp, prop); err != nil {
		return nil, fmt.Errorf("mongo connect: %w", err)
	}

	client, err := drivermongo.Connect(clientOptions)
	if err != nil {
		return nil, fmt.Errorf("mongo connect: %w", err)
	}
	return client, nil
}

// Instrument attaches o11y MongoDB tracing and operation-duration metrics to
// opts without taking over client construction.
//
// opts, tp, mp, and prop are required. prop is accepted to keep the integration
// surface aligned with other o11y facades and to reserve the explicit
// propagation dependency for future outbox/envelope helpers; the contrib
// CommandMonitor itself does not inject trace context into MongoDB documents.
//
// If opts already has a CommandMonitor, Instrument composes it with the o11y
// monitor so both receive Started, Succeeded, and Failed callbacks. Calling
// opts.SetMonitor after Instrument replaces the composed monitor and drops o11y
// instrumentation.
//
// The returned cleanup function is a no-op in the current phase. It exists so a
// future pool-metric observer can unregister its callback without changing this
// API.
func Instrument(
	opts *options.ClientOptions,
	tp trace.TracerProvider,
	mp metric.MeterProvider,
	prop propagation.TextMapPropagator,
) (func(context.Context) error, error) {
	if opts == nil {
		return nil, errors.New("client options must not be nil")
	}
	if tp == nil {
		return nil, errors.New("tracer provider must not be nil")
	}
	if mp == nil {
		return nil, errors.New("meter provider must not be nil")
	}
	if prop == nil {
		return nil, errors.New("propagator must not be nil")
	}

	opts.SetMonitor(composeCommandMonitors(opts.Monitor, NewMonitor(tp, mp)))
	return func(context.Context) error { return nil }, nil
}

// NewMonitor creates the MongoDB CommandMonitor used by this package.
//
// Callers should pass non-nil providers. Nil providers are converted to no-op
// providers instead of allowing otelmongo to fall back to OpenTelemetry globals.
func NewMonitor(tp trace.TracerProvider, mp metric.MeterProvider) *event.CommandMonitor {
	if tp == nil {
		tp = tracenoop.NewTracerProvider()
	}
	if mp == nil {
		mp = metricnoop.NewMeterProvider()
	}
	return otelmongo.NewMonitor(
		otelmongo.WithTracerProvider(tp),
		otelmongo.WithMeterProvider(mp),
	)
}

func composeCommandMonitors(first, second *event.CommandMonitor) *event.CommandMonitor {
	switch {
	case first == nil:
		return second
	case second == nil:
		return first
	default:
		return &event.CommandMonitor{
			Started: func(ctx context.Context, evt *event.CommandStartedEvent) {
				if first.Started != nil {
					first.Started(ctx, evt)
				}
				if second.Started != nil {
					second.Started(ctx, evt)
				}
			},
			Succeeded: func(ctx context.Context, evt *event.CommandSucceededEvent) {
				if first.Succeeded != nil {
					first.Succeeded(ctx, evt)
				}
				if second.Succeeded != nil {
					second.Succeeded(ctx, evt)
				}
			},
			Failed: func(ctx context.Context, evt *event.CommandFailedEvent) {
				if first.Failed != nil {
					first.Failed(ctx, evt)
				}
				if second.Failed != nil {
					second.Failed(ctx, evt)
				}
			},
		}
	}
}
