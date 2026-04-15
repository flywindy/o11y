// Package httpmw provides a net/http middleware that records Golden Signal
// metrics (latency, traffic, errors) for every request handled by the
// wrapped handler. It deliberately does not depend on any HTTP framework
// so it can wrap any stdlib-compatible handler.
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
package httpmw

import (
	"net/http"
	"strconv"
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
// relies entirely on the cardinality cap for protection.
func WithPathNormalizer(fn PathNormalizer) Option {
	return func(c *config) {
		c.normalizer = fn
	}
}

// WithMaxUniquePaths overrides the hard cardinality cap. Values <= 0 mean
// "use DefaultMaxUniquePaths".
func WithMaxUniquePaths(n int) Option {
	return func(c *config) {
		c.maxUniquePaths = n
	}
}

// New returns a net/http middleware that records request duration on the
// supplied meter. The histogram is created once at construction time so
// there is no per-request instrument lookup on the hot path.
func New(meter metric.Meter, opts ...Option) func(http.Handler) http.Handler {
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
		// A histogram creation failure would make the middleware useless;
		// fall back to a no-op wrapper rather than panicking. Callers who
		// care about this can inspect the meter provider directly.
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
				attribute.String("http.response.status_code", strconv.Itoa(rec.status)),
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
