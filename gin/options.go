package gin

import (
	"net/http"
	"strings"

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

// DefaultSkipPaths returns the default list of exact paths excluded from
// tracing by [WithSkipPaths]. It covers the most common Kubernetes liveness,
// readiness, and metrics scrape endpoints. Callers may inspect or extend the
// slice before passing individual paths to [WithFilter].
func DefaultSkipPaths() []string {
	return []string{
		"/health",
		"/healthz",
		"/livez",
		"/readyz",
		"/metrics",
		"/ping",
		"/ready",
		"/live",
	}
}

// SkipPathsOption configures [WithSkipPaths].
type SkipPathsOption interface {
	applySkipPaths(*skipPathsConfig)
}

type skipPathsConfig struct {
	prefixes []string
}

type skipPathsFn func(*skipPathsConfig)

func (f skipPathsFn) applySkipPaths(cfg *skipPathsConfig) { f(cfg) }

// WithSkipPathPrefixes opts in to prefix-based skipping in addition to the
// [DefaultSkipPaths] exact list. Requests whose path starts with any of the
// given prefixes are excluded from tracing.
//
// This is the recommended way to handle sub-path probe conventions such as
// /health/probe or /health/live:
//
//	o11ygin.WithSkipPaths(o11ygin.WithSkipPathPrefixes("/health/"))
func WithSkipPathPrefixes(prefixes ...string) SkipPathsOption {
	return skipPathsFn(func(cfg *skipPathsConfig) {
		cfg.prefixes = append(cfg.prefixes, prefixes...)
	})
}

// WithSkipPaths returns an [Option] that excludes common infra endpoints from
// tracing. By default the paths in [DefaultSkipPaths] are matched exactly.
//
// Pass [WithSkipPathPrefixes] to also exclude paths by prefix:
//
//	o11ygin.Middleware(svc, tp, mp, prop, o11ygin.WithSkipPaths())
//	o11ygin.Middleware(svc, tp, mp, prop, o11ygin.WithSkipPaths(o11ygin.WithSkipPathPrefixes("/health/", "/internal/")))
func WithSkipPaths(opts ...SkipPathsOption) Option {
	cfg := &skipPathsConfig{}
	for _, o := range opts {
		if o == nil {
			continue
		}
		o.applySkipPaths(cfg)
	}
	defaults := DefaultSkipPaths()
	exact := make(map[string]struct{}, len(defaults))
	for _, p := range defaults {
		exact[p] = struct{}{}
	}
	var prefixes []string
	for _, prefix := range cfg.prefixes {
		if prefix != "" {
			prefixes = append(prefixes, prefix)
		}
	}
	return WithFilter(func(r *http.Request) bool {
		p := r.URL.Path
		if _, ok := exact[p]; ok {
			return false
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(p, prefix) {
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
