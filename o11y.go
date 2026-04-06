package o11y

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/flywindy/o11y/internal/log"
	"github.com/flywindy/o11y/internal/trace"
)

// Init initializes the o11y SDK with the provided options.
// It returns a shutdown function to gracefully close providers and an error if initialization fails.
func Init(ctx context.Context, opts ...Option) (func(context.Context), error) {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(cfg)
	}

	if cfg.serviceName == "" {
		return nil, errors.New("service name is required (use WithServiceName)")
	}

	// 1. Initialize Tracing
	traceShutdown, err := trace.InitTracer(ctx, cfg.serviceName, cfg.environment, cfg.otlpEndpoint)
	if err != nil {
		return nil, err
	}

	// 2. Initialize Logging (slog with OTel correlation)
	jsonHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.logLevel,
	})
	otelHandler := log.NewOTelHandler(jsonHandler)
	slog.SetDefault(slog.New(otelHandler))

	// Combined shutdown function
	shutdown := func(shutdownCtx context.Context) {
		traceShutdown(shutdownCtx)
	}

	return shutdown, nil
}
