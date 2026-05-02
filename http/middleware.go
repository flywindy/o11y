// Package http provides instrumentation for net/http servers and clients.
// The middleware records Golden Signal metrics (latency, traffic, errors) for
// every request handled by the wrapped handler and does not depend on any
// HTTP framework — it wraps any stdlib-compatible handler.
//
// The middleware emits a single histogram, http.server.request.duration,
// which the Prometheus exporter renders as http_server_request_duration_seconds.
// The histogram's _count series doubles as the traffic+error counter, so no
// separate counter is emitted.
//
// Cardinality is controlled in two layers:
//
//  1. A user-supplied WithPathNormalizer should collapse dynamic segments
//     to route templates (e.g. /users/123 -> /users/:id). Supply one from
//     your router (chi.RouteContext.RoutePattern, gin.Context.FullPath, ...).
//  2. If no normalizer is given, or the normalizer still produces an
//     unbounded set of paths, a hard cap collapses everything beyond
//     maxUniquePaths to the literal label "other". This protects Prometheus
//     from unbounded cardinality explosion even if the caller forgets
//     step (1).
//
// The wrapper preserves optional ResponseWriter capabilities. It only
// advertises http.Flusher / http.Hijacker / io.ReaderFrom to the handler when
// the underlying writer actually implements them, so feature-detection by
// type-assertion (the legacy pattern still used by net/http itself, gin,
// echo and many other frameworks) returns the correct answer. Unwrap is
// always exposed so http.ResponseController can walk the chain.
//
// Handler panics are converted to a status_code=500 metric sample and then
// re-raised so net/http's default panic handling still runs.
package http

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
)

// DefaultMaxUniquePaths is the default upper bound on distinct http.route
// label values tracked per middleware instance. Requests with a path that
// would push the set over this limit are recorded under http.route="other".
const DefaultMaxUniquePaths = 1000

// PathNormalizer turns a request into its route template. Implementations
// must be safe for concurrent use.
type PathNormalizer func(*http.Request) string

// Option configures the middleware created by New.
type Option func(*config)

type config struct {
	normalizer     PathNormalizer
	maxUniquePaths int
}

// WithPathNormalizer supplies a function that turns a request into its
// route template. Without one, the middleware uses r.URL.Path as-is.
// The provided function must be safe for concurrent use.
func WithPathNormalizer(fn PathNormalizer) Option {
	return func(c *config) {
		c.normalizer = fn
	}
}

// WithMaxUniquePaths overrides the hard cardinality cap. Values <= 0 mean
// "use DefaultMaxUniquePaths". The cap bounds distinct http.route labels.
func WithMaxUniquePaths(n int) Option {
	return func(c *config) {
		c.maxUniquePaths = n
	}
}

// New returns a net/http middleware that records request duration on the
// supplied meter. The histogram is created once at construction time.
//
// New applies the provided Option values to configure a PathNormalizer (defaults to
// r.URL.Path) and a maximum distinct-route cap (defaults to DefaultMaxUniquePaths).
// It constructs a Float64Histogram instrument at creation time; if histogram
// creation fails the error is logged via slog and New returns a no-op wrapper
// that leaves handlers unmodified.
//
// The returned middleware records the request duration in seconds and attaches the
// following attributes: "http.request.method", "http.route" (normalized and capped
// by the configured limit, unseen extra routes are reported as "other"), and
// "http.response.status_code". The response status defaults to 200 if no explicit
// status header is written; if the handler panics before writing a header, the
// metric is recorded with status_code=500 and the panic is re-raised so the
// http.Server's default recovery still runs.
func New(ctx context.Context, meter metric.Meter, opts ...Option) func(http.Handler) http.Handler {
	cfg := &config{
		maxUniquePaths: DefaultMaxUniquePaths,
	}
	for _, opt := range opts {
		opt(cfg)
	}
	if cfg.maxUniquePaths <= 0 {
		cfg.maxUniquePaths = DefaultMaxUniquePaths
	}
	if cfg.normalizer == nil {
		cfg.normalizer = func(r *http.Request) string { return r.URL.Path }
	}

	hist, err := meter.Float64Histogram(
		"http.server.request.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Duration of HTTP server requests."),
	)
	if err != nil {
		slog.ErrorContext(ctx, "http middleware: failed to create histogram, metrics disabled",
			slog.Any("error", err))
		return func(next http.Handler) http.Handler { return next }
	}

	limiter := newPathLimiter(cfg.maxUniquePaths)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec, wrapped := wrapResponseWriter(w)

			defer func() {
				panicValue := recover()
				if panicValue != nil && !rec.wroteHeader {
					rec.status = http.StatusInternalServerError
				}
				hist.Record(r.Context(), time.Since(start).Seconds(),
					metric.WithAttributes(
						semconv.HTTPRequestMethodKey.String(r.Method),
						semconv.HTTPRouteKey.String(limiter.observe(cfg.normalizer(r))),
						semconv.HTTPResponseStatusCodeKey.Int(rec.status),
					))
				if panicValue != nil {
					// Re-raise so the http.Server's default panic handler runs
					// (writes a 500 if nothing was sent and logs the stack via
					// Server.ErrorLog). Using the original value preserves
					// http.ErrAbortHandler semantics.
					panic(panicValue)
				}
			}()

			next.ServeHTTP(wrapped, r)
		})
	}
}

// statusRecorder captures the response status code so we can label metrics
// with it. It defaults to 200 to match Go's implicit first-Write behavior
// when a handler never calls WriteHeader. It deliberately does not implement
// any optional ResponseWriter interfaces directly; wrapResponseWriter selects
// a concrete wrapper variant that exposes only the optional interfaces the
// underlying writer actually supports.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.wroteHeader = true
	}
	return s.ResponseWriter.Write(b)
}

// Unwrap exposes the underlying writer so http.ResponseController (Go 1.20+)
// can walk the wrapper chain to access SetReadDeadline / SetWriteDeadline /
// EnableFullDuplex / etc.
func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}

// wrapResponseWriter returns a *statusRecorder for inspection alongside an
// http.ResponseWriter that conditionally implements http.Flusher,
// http.Hijacker, and io.ReaderFrom — but only the interfaces the underlying
// writer actually supports. This preserves the contract of the legacy
// type-assertion feature-detection pattern used by handlers and frameworks
// (e.g. net/http itself uses io.ReaderFrom for sendfile-style copies, SSE
// handlers use http.Flusher, WebSocket upgraders use http.Hijacker).
func wrapResponseWriter(w http.ResponseWriter) (*statusRecorder, http.ResponseWriter) {
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	_, hasFlusher := w.(http.Flusher)
	_, hasHijacker := w.(http.Hijacker)
	_, hasReaderFrom := w.(io.ReaderFrom)

	switch {
	case hasFlusher && hasHijacker && hasReaderFrom:
		return rec, recFHR{rec}
	case hasFlusher && hasHijacker:
		return rec, recFH{rec}
	case hasFlusher && hasReaderFrom:
		return rec, recFR{rec}
	case hasHijacker && hasReaderFrom:
		return rec, recHR{rec}
	case hasFlusher:
		return rec, recF{rec}
	case hasHijacker:
		return rec, recH{rec}
	case hasReaderFrom:
		return rec, recR{rec}
	default:
		return rec, rec
	}
}

// The recX wrapper types below embed *statusRecorder so they inherit Header,
// Write, WriteHeader, and Unwrap. Each variant adds only the optional
// interface methods the underlying writer is known to support, so a
// `w.(http.Flusher)` type assertion in user code reflects reality.

type recF struct{ *statusRecorder }

func (r recF) Flush() { r.ResponseWriter.(http.Flusher).Flush() }

type recH struct{ *statusRecorder }

func (r recH) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return r.ResponseWriter.(http.Hijacker).Hijack()
}

type recR struct{ *statusRecorder }

func (r recR) ReadFrom(src io.Reader) (int64, error) {
	if !r.wroteHeader {
		r.wroteHeader = true
	}
	return r.ResponseWriter.(io.ReaderFrom).ReadFrom(src)
}

type recFH struct{ *statusRecorder }

func (r recFH) Flush() { r.ResponseWriter.(http.Flusher).Flush() }
func (r recFH) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return r.ResponseWriter.(http.Hijacker).Hijack()
}

type recFR struct{ *statusRecorder }

func (r recFR) Flush() { r.ResponseWriter.(http.Flusher).Flush() }
func (r recFR) ReadFrom(src io.Reader) (int64, error) {
	if !r.wroteHeader {
		r.wroteHeader = true
	}
	return r.ResponseWriter.(io.ReaderFrom).ReadFrom(src)
}

type recHR struct{ *statusRecorder }

func (r recHR) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return r.ResponseWriter.(http.Hijacker).Hijack()
}
func (r recHR) ReadFrom(src io.Reader) (int64, error) {
	if !r.wroteHeader {
		r.wroteHeader = true
	}
	return r.ResponseWriter.(io.ReaderFrom).ReadFrom(src)
}

type recFHR struct{ *statusRecorder }

func (r recFHR) Flush() { r.ResponseWriter.(http.Flusher).Flush() }
func (r recFHR) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return r.ResponseWriter.(http.Hijacker).Hijack()
}
func (r recFHR) ReadFrom(src io.Reader) (int64, error) {
	if !r.wroteHeader {
		r.wroteHeader = true
	}
	return r.ResponseWriter.(io.ReaderFrom).ReadFrom(src)
}

// pathLimiter enforces a hard upper bound on the distinct values that can
// appear in the http.route label. Everything over the cap collapses to
// "other". It is concurrent-safe and lock-free on the hot path once the
// cap has been reached.
type pathLimiter struct {
	max  int
	seen sync.Map
	mu   sync.Mutex
	size int
}

// newPathLimiter creates a pathLimiter that enforces a maximum of n distinct
// route strings. Once that bound is reached, additional unseen paths are
// treated as the literal "other".
func newPathLimiter(n int) *pathLimiter {
	return &pathLimiter{max: n}
}

func (p *pathLimiter) observe(path string) string {
	if _, ok := p.seen.Load(path); ok {
		return path
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.seen.Load(path); ok {
		return path
	}
	if p.size >= p.max {
		return "other"
	}
	p.seen.Store(path, struct{}{})
	p.size++
	return path
}
