package http

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
)

// Option configures HTTP server and client instrumentation.
type Option = otelhttp.Option

// WithSpanNameFormatter customizes span names for HTTP instrumentation.
func WithSpanNameFormatter(f func(operation string, r *http.Request) string) Option {
	return otelhttp.WithSpanNameFormatter(f)
}

// WithPublicEndpoint marks all inbound requests as public endpoints.
func WithPublicEndpoint() Option {
	return otelhttp.WithPublicEndpointFn(func(*http.Request) bool { return true })
}

// WithFilter excludes requests for which f returns false.
func WithFilter(f func(*http.Request) bool) Option {
	return otelhttp.WithFilter(f)
}

// WithMetricAttributesFn adds request-derived metric attributes.
//
// Deprecated: otelhttp deprecates this option in favor of its Labeler API.
// It remains exposed here so existing otelhttp migration recipes can be
// threaded through the facade without importing otelhttp directly.
func WithMetricAttributesFn(f func(*http.Request) []attribute.KeyValue) Option {
	return otelhttp.WithMetricAttributesFn(f)
}
