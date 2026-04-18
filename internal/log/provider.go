package log

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
)

// InitLogger initialises an OTLP/HTTP LoggerProvider backed by a BatchProcessor.
// It shares the provided Resource with the TracerProvider so that service-identity
// attributes (service.name, deployment.environment, etc.) are consistent across
// traces and logs without being duplicated as per-record attributes.
// The returned provider must be shut down via Shutdown when no longer needed.
func InitLogger(ctx context.Context, endpoint string, res *resource.Resource) (*sdklog.LoggerProvider, error) {
	// otlploghttp.WithEndpointURL does not append a default path when none is
	// provided (unlike otlptracehttp). Explicitly set /v1/logs so that a bare
	// endpoint like "http://localhost:4318" routes correctly to the collector.
	logEndpoint, err := logEndpointURL(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid OTLP endpoint %q: %w", endpoint, err)
	}
	exp, err := otlploghttp.New(ctx, otlploghttp.WithEndpointURL(logEndpoint))
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP log exporter: %w", err)
	}

	// Guard: shut down the exporter if provider construction fails so we do not
	// leak the underlying HTTP client or background flush goroutine.
	var initSucceeded bool
	defer func() {
		if !initSucceeded {
			_ = exp.Shutdown(ctx)
		}
	}()

	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)),
		sdklog.WithResource(res),
	)

	initSucceeded = true
	return lp, nil
}

// logEndpointURL returns the full OTLP log endpoint URL.
// If the caller-supplied endpoint has no path (or only "/"), the default
// /v1/logs path is appended. This mirrors the behaviour of otlptracehttp,
// which applies its /v1/traces default automatically.
func logEndpointURL(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if u.Path == "" {
		u.Path = "/v1/logs"
	}
	return u.String(), nil
}
