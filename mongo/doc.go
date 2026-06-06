// Package mongo provides MongoDB tracing, operation-duration metrics, and
// SDK-owned connection-pool metrics.
// Tier: T2 facade over go.opentelemetry.io/contrib/instrumentation/go.mongodb.org/mongo-driver/v2/mongo/otelmongo
// Tier: T3 SDK-owned pool-metric observer - see ADR 0014
package mongo
