// Package mongo wires the o11y SDK's TracerProvider and MeterProvider into the
// official OpenTelemetry MongoDB driver instrumentation without relying on
// global OpenTelemetry state.
package mongo

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
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

// Option configures MongoDB instrumentation behavior.
type Option func(*config)

type config struct {
	poolName string
}

// WithPoolName sets the db.client.connection.pool.name attribute for MongoDB
// connection-pool metrics.
//
// When unset, the SDK derives a bounded name from the first configured MongoDB
// host and the ClientOptions instance identity. The option only affects
// SDK-owned pool metrics; command spans and db.client.operation.duration
// metrics come from otelmongo.
func WithPoolName(name string) Option {
	return func(cfg *config) {
		cfg.poolName = strings.TrimSpace(name)
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
	opts ...Option,
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
	cleanup, err := Instrument(clientOptions, tp, mp, prop, opts...)
	if err != nil {
		return nil, fmt.Errorf("mongo connect: %w", err)
	}

	client, err := drivermongo.Connect(clientOptions)
	if err != nil {
		_ = cleanup(context.Background())
		return nil, fmt.Errorf("mongo connect: %w", err)
	}
	return client, nil
}

// Instrument attaches o11y MongoDB tracing, operation-duration metrics, and
// connection-pool metrics to opts without taking over client construction.
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
// The returned cleanup function disables SDK-owned pool metrics handling for
// these ClientOptions. Applications that build their own ClientOptions should
// defer it near client.Disconnect, after the application's final metrics flush
// if it needs to export a last zero-value pool snapshot.
func Instrument(
	opts *options.ClientOptions,
	tp trace.TracerProvider,
	mp metric.MeterProvider,
	prop propagation.TextMapPropagator,
	instrumentOpts ...Option,
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

	cfg := newConfig(instrumentOpts)
	poolMonitor, cleanup, err := newPoolMonitor(opts, mp, cfg.poolName)
	if err != nil {
		return nil, err
	}

	opts.SetMonitor(composeCommandMonitors(opts.Monitor, NewMonitor(tp, mp)))
	opts.SetPoolMonitor(composePoolMonitors(opts.PoolMonitor, poolMonitor))

	return cleanup, nil
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
		otelmongo.WithSpanNameFormatter(spanName),
	)
}

// mongoSystemName is the db.system.name value this package emits (see
// docs/semconv.md). It also prefixes span names so every o11y data-store
// span shares the "{system}.{operation} {target}" shape (redis.GET,
// s3.PutObject media, mongodb.find users).
const mongoSystemName = "mongodb"

// spanName renders a MongoDB command as "mongodb.{operation} {collection}"
// (e.g. "mongodb.find users"). Commands that target no collection — ping,
// hello, getMore, and admin commands — omit the target: "mongodb.ping".
//
// This replaces otelmongo's default "{collection}.{operation}" so the
// operation leads and the system is identifiable without opening the span.
func spanName(evt *event.CommandStartedEvent) string {
	if evt == nil {
		return mongoSystemName
	}
	if collection := commandCollection(evt); collection != "" {
		return mongoSystemName + "." + evt.CommandName + " " + collection
	}
	return mongoSystemName + "." + evt.CommandName
}

// commandCollection extracts the target collection from a command event.
//
// It deliberately mirrors otelmongo's own (unexported) extractCollection:
// the formatter callback receives only the raw CommandStartedEvent, not the
// collection otelmongo resolves internally, so we re-derive it here. In the
// MongoDB wire protocol the command document's first element carries the
// command name as its key and the collection as its string value
// (e.g. {"find": "users", ...}); commands without a collection put a
// non-string there (e.g. {"getMore": <int64>}, {"ping": 1}), which yields
// an empty result. This is stable wire-format structure, but if upstream
// ever changes how it resolves collections, keep this in sync so the span
// name and db.collection.name attribute agree.
func commandCollection(evt *event.CommandStartedEvent) string {
	elt, err := evt.Command.IndexErr(0)
	if err != nil {
		return ""
	}
	key, err := elt.KeyErr()
	if err != nil || key != evt.CommandName {
		return ""
	}
	val, err := elt.ValueErr()
	if err != nil || val.Type != bson.TypeString {
		return ""
	}
	return val.StringValue()
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

func composePoolMonitors(first, second *event.PoolMonitor) *event.PoolMonitor {
	switch {
	case first == nil:
		return second
	case second == nil:
		return first
	default:
		return &event.PoolMonitor{
			Event: func(evt *event.PoolEvent) {
				if first.Event != nil {
					first.Event(evt)
				}
				if second.Event != nil {
					second.Event(evt)
				}
			},
		}
	}
}
