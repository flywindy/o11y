package gin

import (
	"net/http"

	ginframework "github.com/gin-gonic/gin"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	"go.opentelemetry.io/otel/attribute"
)

// Option configures gin instrumentation.
type Option = otelgin.Option

// WithSpanNameFormatter customizes span names for gin instrumentation.
func WithSpanNameFormatter(f func(*ginframework.Context) string) Option {
	return otelgin.WithSpanNameFormatter(f)
}

// WithFilter excludes requests for which any filter returns false.
func WithFilter(filters ...func(*http.Request) bool) Option {
	otelFilters := make([]otelgin.Filter, 0, len(filters))
	for _, filter := range filters {
		otelFilters = append(otelFilters, otelgin.Filter(filter))
	}
	return otelgin.WithFilter(otelFilters...)
}

// WithMetricAttributesFn adds request-derived metric attributes.
func WithMetricAttributesFn(f func(*http.Request) []attribute.KeyValue) Option {
	return otelgin.WithMetricAttributeFn(f)
}
