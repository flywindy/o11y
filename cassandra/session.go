package cassandra

import (
	"context"
	"errors"

	"github.com/gocql/gocql"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"
)

// NewSession builds an instrumented *gocql.Session from a caller-supplied
// *gocql.ClusterConfig.
//
// The SDK creates the session (rather than wrapping an existing one) because
// gocql attaches observers via the ClusterConfig and cannot attach them to a
// live session after the fact (ADR 0019 §3). The caller keeps full control of
// contact points, consistency, auth, pool sizing, and timeouts on the cluster
// config; NewSession sets (and, if the caller already configured one, composes
// with) the query and connect observers before creating the session.
//
// tp and mp are required and rejected if nil — the package never falls back to
// the OpenTelemetry globals (ADR 0003).
//
// There is no context.Context parameter: gocql v1.7.0's session constructor
// takes no context and builds its own from context.TODO() internally, so a
// caller-supplied context could not bound the initial dial. Dial bounds come
// from the ClusterConfig (ConnectTimeout / Timeout) instead (ADR 0019 §3).
//
// Batch instrumentation is not wired through the driver's BatchObserver: the
// v1.7.0 batch-observer payload carries no batch identity, so it cannot
// faithfully produce one span per logical batch. Use ExecuteBatch for
// instrumented batch execution (ADR 0019 §4).
func NewSession(
	cluster *gocql.ClusterConfig,
	tp trace.TracerProvider,
	mp metric.MeterProvider,
	opts ...Option,
) (*gocql.Session, error) {
	if cluster == nil {
		return nil, errors.New("cassandra: cluster config must not be nil")
	}
	if tp == nil {
		return nil, errors.New("cassandra: tracer provider must not be nil")
	}
	if mp == nil {
		return nil, errors.New("cassandra: meter provider must not be nil")
	}

	obs, err := newObserver(cluster, tp, mp, opts)
	if err != nil {
		return nil, err
	}

	// Compose with any observers the caller already configured so adopting this
	// package does not silently drop their logging/metrics. CreateSession copies
	// the ClusterConfig by value, so the session keeps these composed observers;
	// we restore the caller's originals on the config afterward (on both paths)
	// so a retried or repeated NewSession on the same *ClusterConfig does not
	// stack another SDK observer layer and emit duplicate spans/metrics.
	origQuery := cluster.QueryObserver
	origConnect := cluster.ConnectObserver
	cluster.QueryObserver = chainQueryObserver(origQuery, obs)
	cluster.ConnectObserver = chainConnectObserver(origConnect, obs)

	session, err := cluster.CreateSession()
	cluster.QueryObserver = origQuery
	cluster.ConnectObserver = origConnect
	if err != nil {
		return nil, err
	}
	// Register the session so ExecuteBatch can recover the observer (the
	// SDK-owned batch seam, ADR 0019 §4).
	registerSession(session, obs)
	return session, nil
}

// newObserver assembles the shared observer from the SDK providers and cluster
// contact points. It is split out so unit tests can build an observer without a
// live cluster.
func newObserver(
	cluster *gocql.ClusterConfig,
	tp trace.TracerProvider,
	mp metric.MeterProvider,
	opts []Option,
) (*observer, error) {
	meter := mp.Meter(instrumentationName, metric.WithSchemaURL(semconv.SchemaURL))
	inst, err := newInstruments(meter)
	if err != nil {
		return nil, err
	}
	return &observer{
		tracer: tp.Tracer(instrumentationName, trace.WithSchemaURL(semconv.SchemaURL)),
		inst:   inst,
		cfg:    newConfig(opts),
		server: contactPoint(cluster.Hosts, cluster.Port),
	}, nil
}

// chainQueryObserver composes a caller-supplied QueryObserver with the SDK's, so
// adopting this package does not silently replace a QueryObserver the caller
// already configured on the ClusterConfig. The existing observer runs first.
func chainQueryObserver(existing, sdk gocql.QueryObserver) gocql.QueryObserver {
	if existing == nil {
		return sdk
	}
	return chainedQueryObserver{existing: existing, sdk: sdk}
}

type chainedQueryObserver struct {
	existing gocql.QueryObserver
	sdk      gocql.QueryObserver
}

func (c chainedQueryObserver) ObserveQuery(ctx context.Context, q gocql.ObservedQuery) {
	c.existing.ObserveQuery(ctx, q)
	c.sdk.ObserveQuery(ctx, q)
}

// chainConnectObserver mirrors chainQueryObserver for the connect observer.
func chainConnectObserver(existing, sdk gocql.ConnectObserver) gocql.ConnectObserver {
	if existing == nil {
		return sdk
	}
	return chainedConnectObserver{existing: existing, sdk: sdk}
}

type chainedConnectObserver struct {
	existing gocql.ConnectObserver
	sdk      gocql.ConnectObserver
}

func (c chainedConnectObserver) ObserveConnect(o gocql.ObservedConnect) {
	c.existing.ObserveConnect(o)
	c.sdk.ObserveConnect(o)
}
