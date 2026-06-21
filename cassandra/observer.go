package cassandra

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/gocql/gocql"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"
)

const instrumentationName = "github.com/flywindy/o11y/cassandra"

// attemptKey records the driver-side attempt index on a per-attempt span. gocql
// fires ObserveQuery once per attempt (and per page), so sibling spans carry an
// increasing index, making retries and token-aware host changes visible
// (ADR 0019 §4). There is no stable semconv key for this, so it is SDK-named.
const attemptKey = attribute.Key("cassandra.query.attempt")

// observer implements gocql's QueryObserver and ConnectObserver interfaces. It
// is created once per session and shared across all queries; gocql sets it on
// the *gocql.ClusterConfig before the session exists, so there is no
// idempotency or hook-removal concern (ADR 0019 §2).
type observer struct {
	tracer   trace.Tracer
	inst     *instruments
	cfg      config
	server   serverAddr
	poolName string
}

// ObserveQuery emits one CLIENT span per callback. gocql fires this once per
// attempt and once per page (ADR 0019 §4), so a retried, speculative, or paged
// query produces sibling attempt spans, each a completed Start/End snapshot for
// the host actually contacted. The span uses the observation's own timestamps
// and is parented to the caller's context.
func (o *observer) ObserveQuery(ctx context.Context, q gocql.ObservedQuery) {
	operation, parsedKeyspace, table := parseStatement(q.Statement)

	// db.namespace is the keyspace actually addressed. An explicit keyspace.table
	// qualifier in the statement is authoritative — it overrides the session
	// keyspace, since a qualified statement hits that keyspace regardless of any
	// USE. Only when the statement is unqualified does the driver-reported session
	// keyspace apply (ADR 0019 §5).
	namespace := parsedKeyspace
	if namespace == "" {
		namespace = q.Keyspace
	}

	attrs := o.baseAttrs(operation, namespace, table, q.Host)
	attrs = append(attrs, semconv.DBResponseReturnedRows(q.Rows))
	attrs = append(attrs, attemptKey.Int(q.Attempt))
	if o.cfg.queryTextEnabled && q.Statement != "" {
		attrs = append(attrs, semconv.DBQueryText(q.Statement))
	}

	o.record(ctx, spanName(operation, table), operation, namespace, q.Start, q.End, q.Err, attrs)

	// Increment by a fixed 1 per callback: each callback is exactly one attempt
	// (ADR 0019 §7.B). Never add q.Metrics.Attempts — it is a cumulative
	// per-host snapshot and would over-count retried/paged queries.
	o.inst.attempts.Add(ctx, 1, metric.WithAttributes(o.metricAttrs(operation, namespace, q.Err)...))
}

// ObserveConnect records connect-observer signals (ADR 0019 §7.C): a connection
// attempt counter and, on success, a connection create-time histogram. gocql's
// ObservedConnect carries no context, so the connect duration is recorded
// against context.Background.
//
// server.* is taken from the node actually being dialed (ObservedConnect.Host)
// rather than the configured contact point, so per-node connect failures and
// latencies are attributed to the right host; node count is bounded, so this
// stays cardinality-safe. db.client.connection.pool.name carries the pool label
// semconv recommends on connection metrics.
func (o *observer) ObserveConnect(c gocql.ObservedConnect) {
	ctx := context.Background()
	attrs := []attribute.KeyValue{
		semconv.DBSystemNameCassandra,
		semconv.DBClientConnectionPoolName(o.poolName),
	}
	host, port := o.connectPeer(c.Host)
	if host != "" {
		attrs = append(attrs, semconv.ServerAddress(host))
		if port > 0 {
			attrs = append(attrs, semconv.ServerPort(port))
		}
	}
	if c.Err != nil {
		attrs = append(attrs, semconv.ErrorTypeKey.String(errorType(c.Err)))
	}
	o.inst.connectCount.Add(ctx, 1, metric.WithAttributes(attrs...))
	if c.Err == nil {
		o.inst.connectDuration.Record(ctx, c.End.Sub(c.Start).Seconds(), metric.WithAttributes(attrs...))
	}
}

// connectPeer resolves the node being dialed for a connect observation,
// preferring the actual ObservedConnect.Host and falling back to the configured
// contact point when the driver does not supply a usable address.
func (o *observer) connectPeer(host *gocql.HostInfo) (string, int) {
	if host != nil {
		if ip := host.ConnectAddress(); len(ip) > 0 {
			return ip.String(), host.Port()
		}
	}
	return o.server.host, o.server.port
}

// record creates a completed CLIENT span from a finished observation snapshot
// and records the operation-duration metric. It is shared by the query path and
// the SDK-owned batch seam (ExecuteBatch).
func (o *observer) record(
	ctx context.Context,
	name, operation, keyspace string,
	start, end time.Time,
	obsErr error,
	attrs []attribute.KeyValue,
) {
	// User attributes go first so the SDK's own semconv attributes win on key
	// collisions (OTel resolves duplicate keys last-wins); the built-in keys are
	// part of this package's contract and must not be overridden by
	// WithAttributes.
	spanAttrs := append([]attribute.KeyValue{}, o.cfg.attrs...)
	spanAttrs = append(spanAttrs, attrs...)

	_, span := o.tracer.Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithTimestamp(start),
		trace.WithAttributes(spanAttrs...),
	)

	if obsErr != nil {
		errType := errorType(obsErr)
		// Stamp the exception event at the observation's end time. These are
		// completed snapshots, so the callback's wall-clock now() can be later
		// than the span's End(end), which would order the event after the span.
		span.RecordError(obsErr, trace.WithTimestamp(end))
		span.SetStatus(codes.Error, obsErr.Error())
		span.SetAttributes(semconv.ErrorTypeKey.String(errType))
	}
	span.End(trace.WithTimestamp(end))

	o.inst.operationDuration.Record(ctx, end.Sub(start).Seconds(),
		metric.WithAttributes(o.metricAttrs(operation, keyspace, obsErr)...))
}

// baseAttrs builds the semantic-convention span attributes common to queries
// and batches. host is the actual coordinator (may be nil).
func (o *observer) baseAttrs(operation, keyspace, table string, host *gocql.HostInfo) []attribute.KeyValue {
	attrs := []attribute.KeyValue{semconv.DBSystemNameCassandra}
	if operation != "" {
		attrs = append(attrs, semconv.DBOperationName(operation))
	}
	if keyspace != "" {
		attrs = append(attrs, semconv.DBNamespace(keyspace))
	}
	if table != "" {
		attrs = append(attrs, semconv.DBCollectionName(table))
	}
	attrs = o.appendServerAttrs(attrs)
	// network.peer.* and cassandra.coordinator.* describe the actual contacted
	// coordinator; Opt-In via WithHostAttributes so the package leads with
	// server.* and does not expose per-node addresses by default (ADR 0019 §5).
	if host != nil && o.cfg.hostAttributesEnabled {
		if ip := host.ConnectAddress(); len(ip) > 0 {
			attrs = append(attrs, semconv.NetworkPeerAddress(ip.String()))
			if port := host.Port(); port > 0 {
				attrs = append(attrs, semconv.NetworkPeerPort(port))
			}
		}
		if id := host.HostID(); id != "" {
			attrs = append(attrs, semconv.CassandraCoordinatorID(id))
		}
		if dc := host.DataCenter(); dc != "" {
			attrs = append(attrs, semconv.CassandraCoordinatorDC(dc))
		}
	}
	return attrs
}

// metricAttrs builds the bounded label set for db.client.operation.duration and
// the attempts counter. The MetricViews allow-keys filter is the backstop, but
// keeping the set small here avoids generating high-cardinality streams in the
// first place (ADR 0019 §7).
func (o *observer) metricAttrs(operation, keyspace string, obsErr error) []attribute.KeyValue {
	attrs := []attribute.KeyValue{semconv.DBSystemNameCassandra}
	if operation != "" {
		attrs = append(attrs, semconv.DBOperationName(operation))
	}
	if keyspace != "" {
		attrs = append(attrs, semconv.DBNamespace(keyspace))
	}
	attrs = o.appendServerAttrs(attrs)
	if obsErr != nil {
		attrs = append(attrs, semconv.ErrorTypeKey.String(errorType(obsErr)))
	}
	return attrs
}

// appendServerAttrs appends the logical-server (configured contact point)
// attributes shared by spans, the operation metric, and the connect path. The
// configured contact point is a small fixed set per client, so these are safe
// as metric labels (ADR 0019 §7).
func (o *observer) appendServerAttrs(attrs []attribute.KeyValue) []attribute.KeyValue {
	if o.server.host == "" {
		return attrs
	}
	attrs = append(attrs, semconv.ServerAddress(o.server.host))
	if o.server.port > 0 {
		attrs = append(attrs, semconv.ServerPort(o.server.port))
	}
	return attrs
}

// errorType classifies an observation error for the error.type attribute,
// preferring the stable context sentinels over the concrete Go type name.
func errorType(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "context.DeadlineExceeded"
	case errors.Is(err, context.Canceled):
		return "context.Canceled"
	default:
		if typ := reflect.TypeOf(err); typ != nil {
			return typ.String()
		}
		return "_OTHER"
	}
}
