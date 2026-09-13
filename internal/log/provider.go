package log

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/flywindy/o11y/internal/exportstats"
)

// InitLogger initialises an OTLP/HTTP LoggerProvider backed by a BatchProcessor.
// It shares the provided Resource with the TracerProvider so that service-identity
// attributes (service.name, deployment.environment, etc.) are consistent across
// traces and logs without being duplicated as per-record attributes.
// The returned provider must be shut down via Shutdown when no longer needed.
//
// headers is optional; when non-empty, every OTLP/HTTP request emitted by
// the exporter carries the given headers (used for authentication). failures
// counts every Export call the exporter returned an error for; it may be nil
// when nothing reports the count.
func InitLogger(ctx context.Context, endpoint string, headers map[string]string, res *resource.Resource, failures *exportstats.Recorder) (*sdklog.LoggerProvider, error) {
	// otlploghttp.WithEndpointURL does not append a default path when none is
	// provided (unlike otlptracehttp). Explicitly set /v1/logs so that a bare
	// endpoint like "http://localhost:4318" routes correctly to the collector.
	logEndpoint, err := logEndpointURL(endpoint)
	if err != nil {
		// The value is not repeated: a credential in its userinfo must not
		// reach the Init error, and a *url.Error carries the URL, so only
		// the parser's reason is kept.
		var uerr *url.Error
		if errors.As(err, &uerr) && uerr.Err != nil {
			err = uerr.Err
		}
		return nil, fmt.Errorf("invalid OTLP endpoint: %w", err)
	}
	expOpts := []otlploghttp.Option{otlploghttp.WithEndpointURL(logEndpoint)}
	if len(headers) > 0 {
		expOpts = append(expOpts, otlploghttp.WithHeaders(headers))
	}
	exp, err := otlploghttp.New(ctx, expOpts...)
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

	var batched sdklog.Exporter = exp
	if failures != nil {
		batched = exportstats.LogExporter(exp, failures)
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(batched)),
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
