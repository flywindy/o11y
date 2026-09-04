// Package elasticsearch wires the o11y SDK's TracerProvider into the official
// github.com/elastic/go-elasticsearch/v8 client's first-party OpenTelemetry
// instrumentation, and records an SDK-owned db.client.operation.duration
// histogram on the SDK's MeterProvider, without relying on global OpenTelemetry
// state.
//
// The tracing instrumentation ships inside the client (in the shared
// github.com/elastic/elastic-transport-go/v8 transport), so this package owns
// the wiring and the defaults plus thin response-side normalizations the bare
// upstream lacks: it guards search-body capture against a nil body, and it
// records http.response.status_code and marks the span status = Error for an
// ES HTTP error response (status > 299, matching the client's own IsError) that
// the upstream would otherwise leave UNSET (see ADR 0020 §4). The upstream
// instrumentation is trace-only, so the operation-duration metric is SDK-owned:
// one sample per request, derived from the same instrumentation callbacks and
// labeled with the current semconv v1.39.0 keys (db.system.name,
// db.operation.name, db.collection.name, server.address, server.port,
// error.type) — see ADR 0027. There is no propagator parameter, because the
// upstream does not propagate trace context toward Elasticsearch.
//
// Issuing Elasticsearch operations from background or post-response work (a
// goroutine, a deferred write, fire-and-forget)? Do not pass the request
// context (c.Request.Context() / *gin.Context) — it is canceled when the
// handler returns and aborts the request. Carry it with
// github.com/flywindy/o11y/obsctx (Detach / Go) so the work keeps the trace
// but is not canceled with the request. See ADR 0024 and examples/background.
//
// Tier: T2 facade over the github.com/elastic/go-elasticsearch/v8 first-party
// OpenTelemetry instrumentation for spans; the db.client.operation.duration
// metric is an SDK-owned justified-T3 layer (ADR 0027).
package elasticsearch
