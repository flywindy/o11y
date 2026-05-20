// Package main demonstrates continuous profiling with the o11y SDK.
//
// Prerequisites:
//
//	# kind-config.yaml already maps localhost:4318 to the OTel Collector.
//	kubectl port-forward -n infra svc/alloy 4040:4040
//	kubectl port-forward -n infra svc/grafana 3000:3000
//	go run examples/profiling/main.go
//
// In Grafana (http://localhost:3000), open Explore / Pyroscope and select the
// profiling-example service. In Explore / Tempo, root spans from this example
// should also carry pyroscope.profile.id when sampled CPU work overlaps the
// profiler sampling window.
package main

import (
	"context"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/flywindy/o11y"
)

var cpuSink float64

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	otlpEndpoint := envOrDefault("OTLP_ENDPOINT", "http://localhost:4318")
	profilingEndpoint := envOrDefault("PYROSCOPE_ENDPOINT", "http://localhost:4040")

	obs, err := o11y.Init(ctx,
		o11y.WithServiceName("profiling-example"),
		o11y.WithServiceVersion("0.1.0"),
		o11y.WithEnvironment("development"),
		o11y.WithServiceNamespace("platform"),
		o11y.WithOTLPEndpoint(otlpEndpoint),
		o11y.WithMetricsOTLPEndpoint(otlpEndpoint),
		o11y.WithProfilingEndpoint(profilingEndpoint),
		o11y.WithProfilingEnabled(true),
		o11y.WithLogLevel(slog.LevelInfo),
	)
	if err != nil {
		slog.ErrorContext(ctx, "failed to initialize o11y SDK", slog.Any("error", err))
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := obs.Shutdown(shutdownCtx); err != nil {
			obs.Logger.ErrorContext(shutdownCtx, "SDK shutdown error", slog.Any("error", err))
		}
	}()

	obs.Logger.InfoContext(ctx, "profiling example started",
		slog.String("otlp_endpoint", otlpEndpoint),
		slog.String("profiling_endpoint", profilingEndpoint),
	)

	tracer := obs.Tracer("profiling-example")
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for i := 1; ; i++ {
		select {
		case <-ctx.Done():
			obs.Logger.InfoContext(ctx, "profiling example stopped")
			return
		case <-ticker.C:
			workCtx, span := tracer.Start(ctx, "cpu-bound-root-work")
			result := burnCPU(workCtx, 1500*time.Millisecond)
			span.End()

			obs.Logger.InfoContext(workCtx, "completed profiled work",
				slog.Int("iteration", i),
				slog.Float64("result", result),
			)
		}
	}
}

func burnCPU(ctx context.Context, duration time.Duration) float64 {
	deadline := time.Now().Add(duration)
	var total float64
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			break
		}
		for i := 1; i < 10000; i++ {
			total += math.Sqrt(float64(i)) * math.Sin(float64(i))
		}
	}
	cpuSink = total
	return cpuSink
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
