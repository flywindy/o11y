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
