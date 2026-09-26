// Package gin provides gin instrumentation wrappers for the o11y SDK.
// Tier: T2 facade over go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin
//
// Middleware returns the canonical chain:
//
//	r.Use(o11ygin.Middleware("svc", tp, mp, prop)...)
//	r.Use(gin.Recovery())
//
// The otelgin middleware opens the server span. The chain's second handler
// records each gin.Context.Errors entry as one exception event carrying the
// gin.error.type attribute, making gin error categories queryable without
// adding high-cardinality metric labels, and keeps c.Errors from otelgin so
// the error is not recorded a second time. gin.Recovery should be registered
// inside that chain so recovered panics still produce complete HTTP status
// attributes and metrics.
//
// ErrorRecorder is for chains that do not use Middleware; there it records
// each error as an exception event carrying gin.error.type.
package gin
