// Package cassandra instruments github.com/gocql/gocql sessions with the o11y
// SDK's tracer and meter providers.
// Tier: T3 SDK-owned observers over github.com/gocql/gocql
//
// The package owns Cassandra span creation, attribute population, and metric
// recording directly, wired to explicit SDK providers with no OpenTelemetry
// global mutation (ADR 0003, ADR 0019). Queries and connections are
// instrumented via the driver's QueryObserver and ConnectObserver, set once on
// the *gocql.ClusterConfig at session creation, so there is no idempotency map
// and no hook-removal problem. Batches are instrumented through the SDK-owned
// ExecuteBatch seam rather than the driver's BatchObserver, whose gocql v1.7.0
// payload cannot identify a logical batch (ADR 0019 §4).
//
// Provenance. The observer skeleton is informed by the abandoned
// go.opentelemetry.io/contrib/.../gocql/otelgocql package (Apache-2.0, removed
// from contrib in v1.19.0). This package does not vendor that code verbatim: it
// is rewritten to emit semconv v1.39.0 attributes (not the pre-stable
// db.cassandra.* / db.name keys), requires explicit tracer/meter providers
// instead of falling back to the OpenTelemetry globals, and aligns metric
// shapes with the SDK's db.client.* contract. The SDK owns and maintains the
// result.
//
// Driver portability. gocql/gocql and the Apache fork
// (github.com/apache/cassandra-gocql-driver/v2) expose the same observer
// interfaces, so the only driver-specific surface here is the import path and
// the NewSession constructor seam. A future "support Apache v2" change is a
// localized port, not a rewrite (ADR 0019 §1).
package cassandra
