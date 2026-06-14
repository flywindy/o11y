// Package elasticsearch wires the o11y SDK's TracerProvider into the official
// github.com/elastic/go-elasticsearch/v8 client's first-party OpenTelemetry
// instrumentation without relying on global OpenTelemetry state.
//
// The instrumentation ships inside the client (in the shared
// github.com/elastic/elastic-transport-go/v8 transport), so this package owns
// only the wiring and the defaults — it emits no spans or attributes of its
// own. The integration is trace-only in v1: there is no MeterProvider
// parameter (see ADR 0020 §6) and no propagator parameter, because the
// upstream instrumentation accepts neither.
//
// Issuing Elasticsearch operations from background or post-response work (a
// goroutine, a deferred write, fire-and-forget)? Do not pass the request
// context (c.Request.Context() / *gin.Context) — it is canceled when the
// handler returns and aborts the request. Carry it with
// github.com/flywindy/o11y/obsctx (Detach / Go) so the work keeps the trace
// but is not canceled with the request. See ADR 0024 and examples/background.
//
// Tier: T2 facade over the github.com/elastic/go-elasticsearch/v8 first-party
// OpenTelemetry instrumentation
package elasticsearch
