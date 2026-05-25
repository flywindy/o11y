package resty

import restyclient "github.com/go-resty/resty/v2"

// Option configures Resty instrumentation behavior.
type Option func(*config)

type config struct {
	routeKey           any
	routeEnabled       bool
	metricRouteEnabled bool
	spanNameFormatter  func(*restyclient.Request) string
}

// WithRouteFromContext configures the wrapper to read a route template from
// request contexts.
//
// When the context value at key is a non-empty string, it is recorded as
// http.route on spans. Pair this option with WithMetricRouteEnabled(true) to
// also emit the same bounded route template on http.client.request.duration.
func WithRouteFromContext(key any) Option {
	return func(cfg *config) {
		cfg.routeKey = key
		cfg.routeEnabled = true
	}
}

// WithMetricRouteEnabled controls whether resolved route templates are
// attached to client duration metric samples.
//
// The default is false so outbound client metric cardinality stays bounded by
// method, host, port, status, and error type. Enable this only when callers set
// low-cardinality templates through WithRouteFromContext.
func WithMetricRouteEnabled(enabled bool) Option {
	return func(cfg *config) {
		cfg.metricRouteEnabled = enabled
	}
}

// WithSpanNameFormatter customizes client span names.
//
// When omitted, spans are named as METHOD or METHOD route when
// WithRouteFromContext resolves a route template.
func WithSpanNameFormatter(f func(*restyclient.Request) string) Option {
	return func(cfg *config) {
		cfg.spanNameFormatter = f
	}
}

func newConfig(opts []Option) config {
	cfg := config{}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return cfg
}
