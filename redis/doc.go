// Package redis instruments github.com/redis/go-redis/v9 clients with the
// o11y SDK's tracer and meter providers.
// Tier: T3 SDK-owned hook over github.com/redis/go-redis/v9
//
// The wrapper owns Redis tracing and metrics directly so emitted telemetry
// follows the SDK's pinned OpenTelemetry semantic conventions without relying
// on package-level OpenTelemetry globals.
package redis
