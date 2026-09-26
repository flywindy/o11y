// Package gin provides gin instrumentation wrappers for the o11y SDK.
// Tier: T2 facade over go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin
//
// Middleware returns the canonical chain:
//
//	r.Use(o11ygin.Middleware("svc", tp, mp, prop)...)
//	r.Use(gin.Recovery())
//
// The otelgin middleware opens the server span and, on unwind, records each
// gin.Context.Errors entry as an exception event. The chain's second handler
// adds one "gin.error" event per entry carrying the gin.error.type attribute
// and gin.error.message, equal to the exception.message of that entry's
// exception event, making gin error categories queryable without a second exception
// event and without adding high-cardinality metric labels. gin.Recovery should be
// registered inside that chain so recovered panics still produce complete HTTP
// status attributes and metrics.
//
// ErrorRecorder is for chains that do not use Middleware; there it records
// each error as an exception event carrying gin.error.type.
package gin
