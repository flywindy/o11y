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
package http

import (
	"bufio"
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
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
// route template. Without one, the middleware uses r.URL.Path as-is and
// The provided function must be safe for concurrent use; the middleware enforces a configured cap and will collapse excess distinct routes to "other".
func WithPathNormalizer(fn PathNormalizer) Option {
	return func(c *config) {
		c.normalizer = fn
	}
}

// WithMaxUniquePaths overrides the hard cardinality cap. Values <= 0 mean
// WithMaxUniquePaths sets the maximum number of distinct http.route label values the middleware will track.
// n is the cap for unique routes; values <= 0 are treated as DefaultMaxUniquePaths when the middleware is constructed.
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
// status header is written.
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
		slog.ErrorContext(ctx, "httpmw: failed to create histogram, metrics disabled",
			slog.Any("error", err))
		return func(next http.Handler) http.Handler { return next }
	}

	limiter := newPathLimiter(cfg.maxUniquePaths)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rec, r)

			route := limiter.observe(cfg.normalizer(r))
			elapsed := time.Since(start).Seconds()

			hist.Record(r.Context(), elapsed, metric.WithAttributes(
				attribute.String("http.request.method", r.Method),
				attribute.String("http.route", route),
				attribute.Int("http.response.status_code", rec.status),
			))
		})
	}
}

// statusRecorder captures the response status code so we can label metrics
// with it. It defaults to 200 to match Go's implicit first-Write behavior
// when a handler never calls WriteHeader.
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

// Flush delegates to the underlying ResponseWriter when it implements
// http.Flusher. Required for SSE and chunked-streaming handlers.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack delegates to the underlying ResponseWriter when it implements
// http.Hijacker. Required for WebSocket upgrades and HTTP/1.1 connection
// hijacking.
func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := s.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
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

// newPathLimiter creates a pathLimiter that enforces a maximum of max distinct route strings.
// max is the upper bound on distinct paths that will be tracked; once that bound is reached,
// additional unseen paths will be treated as the literal `"other"`.
func newPathLimiter(max int) *pathLimiter {
	return &pathLimiter{max: max}
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
