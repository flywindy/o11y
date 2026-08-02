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

// reservedAttrKeys are the keys this package owns on spans and metrics. Values
// supplied via WithAttributes that reuse these are dropped (filterReservedAttrs),
// so a caller cannot override the package's contract or smuggle in a key that is
// only emitted conditionally — e.g. db.query.text while WithQueryText is off (the
// default sensitive-data guard), or error.type on a successful span. Relying on
// last-wins alone would let such a value survive whenever the SDK does not append
// its own.
var reservedAttrKeys = map[attribute.Key]struct{}{
	semconv.DBSystemNameKey:               {},
	semconv.DBNamespaceKey:                {},
	semconv.DBOperationNameKey:            {},
	semconv.DBCollectionNameKey:           {},
	semconv.DBResponseReturnedRowsKey:     {},
	semconv.DBQueryTextKey:                {},
	semconv.DBOperationBatchSizeKey:       {},
	semconv.ServerAddressKey:              {},
	semconv.ServerPortKey:                 {},
	semconv.NetworkPeerAddressKey:         {},
	semconv.NetworkPeerPortKey:            {},
	semconv.CassandraCoordinatorIDKey:     {},
	semconv.CassandraCoordinatorDCKey:     {},
	semconv.ErrorTypeKey:                  {},
	semconv.DBClientConnectionPoolNameKey: {},
	attemptKey:                            {},
}

// filterReservedAttrs returns attrs without any reserved-key entries, leaving the
// caller's slice untouched. Returns the input unchanged when nothing is reserved.
func filterReservedAttrs(attrs []attribute.KeyValue) []attribute.KeyValue {
	for _, a := range attrs {
		if _, reserved := reservedAttrKeys[a.Key]; reserved {
			filtered := make([]attribute.KeyValue, 0, len(attrs))
			for _, a := range attrs {
				if _, reserved := reservedAttrKeys[a.Key]; !reserved {
					filtered = append(filtered, a)
				}
			}
			return filtered
		}
	}
	return attrs
}

// target identifies the database object an observation addressed: the parsed
// statement verb, the keyspace (db.namespace), and the single table
// (db.collection.name) when one can be resolved. The three travel together
// through span-attribute, metric-label, and span-name construction, so they are
// grouped rather than threaded positionally.
type target struct {
	operation string
	keyspace  string
	table     string
}

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
	tgt := target{operation: operation, keyspace: namespace, table: table}

	// server.* is the node that actually ran this attempt (token-aware routing),
	// falling back to the contact point when the driver supplies no host.
	srv := o.queryServer(q.Host)

	attrs := o.baseAttrs(tgt, srv, q.Host)
	attrs = append(attrs, semconv.DBResponseReturnedRows(q.Rows))
	attrs = append(attrs, attemptKey.Int(q.Attempt))
	if o.cfg.queryTextEnabled && q.Statement != "" {
		attrs = append(attrs, semconv.DBQueryText(q.Statement))
	}

	spanCtx := o.record(ctx, spanName(operation, table), tgt, srv, q.Start, q.End, q.Err, attrs)

	// Increment by a fixed 1 per callback: each callback is exactly one attempt
	// (ADR 0019 §7.B). Never add q.Metrics.Attempts — it is a cumulative
	// per-host snapshot and would over-count retried/paged queries. Use the
	// span's context so this counter's exemplar references the query span too.
	o.inst.attempts.Add(spanCtx, 1, metric.WithAttributes(o.metricAttrs(tgt, srv, q.Err)...))
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
	srv := o.queryServer(host)
	return srv.host, srv.port
}

// record creates a completed CLIENT span from a finished observation snapshot
// and records the operation-duration metric. It is shared by the query path and
// the SDK-owned batch seam (ExecuteBatch). It returns the span's context so
// callers can record additional metrics whose exemplars should reference this
// operation's span (e.g. the per-attempt counter).
func (o *observer) record(
	ctx context.Context,
	name string,
	tgt target,
	srv serverAddr,
	start, end time.Time,
	obsErr error,
	attrs []attribute.KeyValue,
) context.Context {
	// User attributes go first so the SDK's own semconv attributes win on key
	// collisions (OTel resolves duplicate keys last-wins); the built-in keys are
	// part of this package's contract and must not be overridden by
	// WithAttributes.
	spanAttrs := append([]attribute.KeyValue{}, o.cfg.attrs...)
	spanAttrs = append(spanAttrs, attrs...)

	spanCtx, span := o.tracer.Start(ctx, name,
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

	// Record against the span's context (not the parent ctx) so the duration
	// metric's exemplar references this operation's CLIENT span rather than the
	// caller's parent span. The span has ended, but its SpanContext is still the
	// correct trace/span id to correlate the data point to.
	o.inst.operationDuration.Record(spanCtx, end.Sub(start).Seconds(),
		metric.WithAttributes(o.metricAttrs(tgt, srv, obsErr)...))

	return spanCtx
}

// baseAttrs builds the semantic-convention span attributes common to queries
// and batches. srv is the server attributed to this observation (the actual
// coordinator for queries, the contact point for batches); host is the actual
// coordinator for the Opt-In topology attributes (may be nil).
func (o *observer) baseAttrs(tgt target, srv serverAddr, host *gocql.HostInfo) []attribute.KeyValue {
	attrs := []attribute.KeyValue{semconv.DBSystemNameCassandra}
	if tgt.operation != "" {
		attrs = append(attrs, semconv.DBOperationName(tgt.operation))
	}
	if tgt.keyspace != "" {
		attrs = append(attrs, semconv.DBNamespace(tgt.keyspace))
	}
	if tgt.table != "" {
		attrs = append(attrs, semconv.DBCollectionName(tgt.table))
	}
	attrs = appendServerAttrs(attrs, srv)
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
// the attempts counter. srv is the server attributed to the observation (the
// actual coordinator for queries; cardinality stays bounded because it is one of
// the cluster's nodes — a fixed set — ADR 0019 §7). The MetricViews allow-keys
// filter is the backstop.
//
// db.collection.name is included by default (ADR 0019 §7 amendment): semconv
// marks it Conditionally Required on db.client.operation.duration "if readily
// available and if a database call is performed on a single collection", and
// both hold here — the table is already parsed for the span, and CQL has no
// joins, so a query addresses one table. It is omitted when the table could not
// be resolved (unparsed statement, or a batch spanning several tables), which is
// exactly the "single collection" condition failing. WithCollectionMetricLabel(false)
// drops it for callers who would rather not pay the per-table series, and
// o11y.WithMaxUniqueCollections caps distinct values at the export boundary so a
// statement shape the parser mis-reads cannot grow the label without bound.
func (o *observer) metricAttrs(tgt target, srv serverAddr, obsErr error) []attribute.KeyValue {
	attrs := []attribute.KeyValue{semconv.DBSystemNameCassandra}
	if tgt.operation != "" {
		attrs = append(attrs, semconv.DBOperationName(tgt.operation))
	}
	if tgt.keyspace != "" {
		attrs = append(attrs, semconv.DBNamespace(tgt.keyspace))
	}
	if tgt.table != "" && o.cfg.collectionMetricLabel() {
		attrs = append(attrs, semconv.DBCollectionName(tgt.table))
	}
	attrs = appendServerAttrs(attrs, srv)
	if obsErr != nil {
		attrs = append(attrs, semconv.ErrorTypeKey.String(errorType(obsErr)))
	}
	return attrs
}

// queryServer resolves the server.* attributed to a query observation: the actual
// coordinator from ObservedQuery.Host (token-aware routing sends a query to the
// replica node, not necessarily the configured contact point), falling back to
// the contact point when the driver supplies no usable host (ADR 0019 §7).
func (o *observer) queryServer(host *gocql.HostInfo) serverAddr {
	if host != nil {
		if ip := host.ConnectAddress(); len(ip) > 0 {
			return serverAddr{host: ip.String(), port: host.Port()}
		}
	}
	return o.server
}

// appendServerAttrs appends the server.address / server.port attributes for the
// given server, shared by spans, the operation metric, and the connect path.
func appendServerAttrs(attrs []attribute.KeyValue, srv serverAddr) []attribute.KeyValue {
	if srv.host == "" {
		return attrs
	}
	attrs = append(attrs, semconv.ServerAddress(srv.host))
	if srv.port > 0 {
		attrs = append(attrs, semconv.ServerPort(srv.port))
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
