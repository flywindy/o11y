// Package mongo provides MongoDB tracing, operation-duration metrics, and
// SDK-owned connection-pool metrics.
//
// Issuing MongoDB operations from background or post-response work (a goroutine,
// a deferred write, fire-and-forget)? Do not pass the request context
// (c.Request.Context() / *gin.Context) — it is canceled when the handler
// returns and aborts the operation (context canceled / connection reset by
// peer). Carry it with github.com/flywindy/o11y/obsctx (Detach / Go) so the
// work keeps the trace but is not canceled with the request. See ADR 0024 and
// examples/background.
//
// Tier: T2 facade over go.opentelemetry.io/contrib/instrumentation/go.mongodb.org/mongo-driver/v2/mongo/otelmongo
// Tier: T3 SDK-owned pool-metric observer - see ADR 0014
package mongo
