// Package resty instruments github.com/go-resty/resty/v2 clients with o11y
// tracing and metrics.
// Tier: T3 SDK-owned hooks over github.com/go-resty/resty/v2
//
// The wrapper owns span lifecycle in resty hooks instead of installing an
// otelhttp transport. This keeps retry attempts observable as individual
// sibling spans and allows resty-specific attributes such as
// resty.error.kind and resty.retry.exhausted to be recorded before spans end.
package resty
