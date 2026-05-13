// Package gin provides gin instrumentation wrappers for the o11y SDK.
// Tier: T2 facade over go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin
//
// Middleware returns the canonical chain:
//
//	r.Use(o11ygin.Middleware("svc", tp, mp, prop)...)
//	r.Use(gin.Recovery())
//
// The otelgin middleware opens the server span, ErrorRecorder observes
// gin.Context.Errors after downstream handlers run, and gin.Recovery should be
// registered inside that chain so recovered panics still produce complete HTTP
// status attributes and metrics.
//
// ErrorRecorder enhances otelgin's c.Errors handling by recording typed error
// events with the gin.error.type attribute, making gin error categories
// queryable without adding high-cardinality metric labels.
package gin
