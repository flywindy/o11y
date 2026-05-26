// Package main demonstrates outbound Resty tracing and metrics through the
// o11y Resty wrapper.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/flywindy/o11y"
	o11yhttp "github.com/flywindy/o11y/http"
	o11yresty "github.com/flywindy/o11y/resty"
	restyclient "github.com/go-resty/resty/v2"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type routeKey struct{}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		slog.ErrorContext(context.Background(), "Resty example failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	obs, err := o11y.Init(ctx,
		o11y.WithServiceName("resty-example"),
		o11y.WithServiceVersion("0.1.0"),
		o11y.WithEnvironment("development"),
		o11y.WithServiceNamespace("platform"),
		o11y.WithOTLPEndpoint("http://localhost:4318"),
		o11y.WithMetricsOTLPEndpoint("http://localhost:4318"),
		o11y.WithLogLevel(slog.LevelInfo),
	)
	if err != nil {
		return fmt.Errorf("initialize o11y SDK: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := obs.Shutdown(shutdownCtx); err != nil {
			obs.Logger.ErrorContext(shutdownCtx, "SDK shutdown error", slog.Any("error", err))
		}
	}()

	downstream, err := startDownstream(ctx, obs)
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := downstream.Shutdown(shutdownCtx); err != nil {
			obs.Logger.ErrorContext(shutdownCtx, "downstream shutdown error", slog.Any("error", err))
		}
	}()

	client := o11yresty.NewClient(
		obs.TracerProvider(),
		obs.MeterProvider(),
		obs.Propagator,
		o11yresty.WithRouteFromContext(routeKey{}),
		o11yresty.WithMetricRouteEnabled(true),
	)
	client.SetBaseURL("http://localhost:8081").
		SetRetryCount(1).
		SetRetryWaitTime(100 * time.Millisecond).
		SetRetryMaxWaitTime(100 * time.Millisecond).
		AddRetryCondition(func(resp *restyclient.Response, _ error) bool {
			return resp != nil && resp.StatusCode() == http.StatusServiceUnavailable
		})

	logger := obs.Logger
	tracer := obs.Tracer("resty-example")
	logger.InfoContext(ctx, "Resty example started",
		slog.String("downstream", "http://localhost:8081"),
		slog.String("metrics_sink", "OTLP -> OTel Collector -> Prometheus"),
	)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		callDownstream(ctx, logger, tracer, client)
		select {
		case <-ctx.Done():
			logger.InfoContext(ctx, "Resty example stopped")
			return nil
		case <-ticker.C:
		}
	}
}

func startDownstream(ctx context.Context, obs *o11y.SDK) (*http.Server, error) {
	var attempts atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n%3 == 1 {
			http.Error(w, "retry me", http.StatusServiceUnavailable)
			return
		}
		obs.Logger.InfoContext(r.Context(), "downstream order lookup completed",
			slog.String("order_id", r.PathValue("id")),
		)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	server := &http.Server{
		Addr: ":8081",
		Handler: o11yhttp.NewServerHandler(
			mux,
			obs.TracerProvider(),
			obs.MeterProvider(),
			obs.Propagator,
		),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		obs.Logger.InfoContext(ctx, "downstream server listening", slog.String("addr", "http://localhost:8081"))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()
	select {
	case err := <-errCh:
		if err != nil {
			return nil, fmt.Errorf("start downstream server: %w", err)
		}
		return nil, errors.New("downstream server stopped")
	case <-time.After(100 * time.Millisecond):
		return server, nil
	}
}

func callDownstream(
	ctx context.Context,
	logger *slog.Logger,
	tracer trace.Tracer,
	client *restyclient.Client,
) {
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	callCtx, span := tracer.Start(callCtx, "resty-cycle")
	defer span.End()

	callCtx = context.WithValue(callCtx, routeKey{}, "/api/orders/{id}")
	resp, err := client.R().
		SetContext(callCtx).
		Get("/api/orders/123")
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		logger.ErrorContext(callCtx, "Resty request failed", slog.Any("error", err))
		return
	}
	if resp.IsError() {
		err := fmt.Errorf("downstream returned %s", resp.Status())
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		logger.ErrorContext(callCtx, "Resty response failed",
			slog.Int("status", resp.StatusCode()),
		)
		return
	}
	logger.InfoContext(callCtx, "Resty request completed",
		slog.Int("status", resp.StatusCode()),
		slog.Duration("duration", resp.Time()),
	)
}
