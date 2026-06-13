package gin

import (
	"net/http"

	ginframework "github.com/gin-gonic/gin"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	"go.opentelemetry.io/otel/attribute"
)

// Option configures gin instrumentation.
type Option interface {
	applyGinOption(*ginOptions)
}

type ginOptions struct {
	otel []otelgin.Option
}

type optionFunc func(*ginOptions)

func (f optionFunc) applyGinOption(opts *ginOptions) {
	f(opts)
}

// WithSpanNameFormatter customizes span names for gin instrumentation.
func WithSpanNameFormatter(f func(*ginframework.Context) string) Option {
	return optionFunc(func(opts *ginOptions) {
		opts.otel = append(opts.otel, otelgin.WithSpanNameFormatter(f))
	})
}

// WithFilter excludes requests for which any filter returns false.
func WithFilter(filters ...func(*http.Request) bool) Option {
	return optionFunc(func(opts *ginOptions) {
		otelFilters := make([]otelgin.Filter, 0, len(filters))
		for _, filter := range filters {
			otelFilters = append(otelFilters, otelgin.Filter(filter))
		}
		opts.otel = append(opts.otel, otelgin.WithFilter(otelFilters...))
	})
}

// WithMetricAttributesFn adds request-derived metric attributes.
//
// It wraps otelgin.WithMetricAttributeFn; the pluralized facade name mirrors
// o11y/http's metric-attribute option naming.
func WithMetricAttributesFn(f func(*http.Request) []attribute.KeyValue) Option {
	return optionFunc(func(opts *ginOptions) {
		opts.otel = append(opts.otel, otelgin.WithMetricAttributeFn(f))
	})
}

// defaultProbeExactPaths are the well-known infra probe paths skipped by
// [WithSkipProbes] by default. These are exact matches only.
var defaultProbeExactPaths = map[string]struct{}{
	"/health":    {},
	"/healthz":   {},
	"/livez":     {},
	"/readyz":    {},
	"/metrics":   {},
	"/ping":      {},
	"/ready":     {},
	"/live":      {},
}

// SkipProbesOption configures [WithSkipProbes].
type SkipProbesOption interface {
	applySkipProbes(*skipProbesConfig)
}

type skipProbesConfig struct {
	prefixes []string
}

type skipProbesFn func(*skipProbesConfig)

func (f skipProbesFn) applySkipProbes(cfg *skipProbesConfig) { f(cfg) }

// WithSkipProbePrefixes opts in to prefix-based skipping in addition to the
// default exact-path list. Requests whose path starts with any of the given
// prefixes are excluded from tracing.
//
// Example:
//
//	o11ygin.WithSkipProbes(o11ygin.WithSkipProbePrefixes("/internal/", "/_"))
func WithSkipProbePrefixes(prefixes ...string) SkipProbesOption {
	return skipProbesFn(func(cfg *skipProbesConfig) {
		cfg.prefixes = append(cfg.prefixes, prefixes...)
	})
}

// WithSkipProbes returns an [Option] that excludes well-known infra probe
// endpoints from tracing. By default only exact paths are matched
// (/health, /healthz, /livez, /readyz, /metrics, /ping, /ready, /live).
//
// Pass [WithSkipProbePrefixes] to also skip paths by prefix:
//
//	o11ygin.Middleware(svc, tp, mp, prop, o11ygin.WithSkipProbes())
//	o11ygin.Middleware(svc, tp, mp, prop, o11ygin.WithSkipProbes(o11ygin.WithSkipProbePrefixes("/internal/")))
func WithSkipProbes(opts ...SkipProbesOption) Option {
	cfg := &skipProbesConfig{}
	for _, o := range opts {
		o.applySkipProbes(cfg)
	}
	prefixes := cfg.prefixes
	return WithFilter(func(r *http.Request) bool {
		p := r.URL.Path
		if _, ok := defaultProbeExactPaths[p]; ok {
			return false
		}
		for _, prefix := range prefixes {
			if len(p) >= len(prefix) && p[:len(prefix)] == prefix {
				return false
			}
		}
		return true
	})
}

func applyOptions(options []Option) []otelgin.Option {
	opts := ginOptions{}
	for _, option := range options {
		if option == nil {
			continue
		}
		option.applyGinOption(&opts)
	}
	return opts.otel
}
