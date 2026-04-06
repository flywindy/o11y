package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/flywindy/o11y/pkg/o11y"
	"go.opentelemetry.io/otel"
)

func main() {
	// 1. Initialize the SDK
	ctx := context.Background()
	cfg := o11y.Config{
		ServiceName:  "basic-example",
		Environment:  "development",
		OTLPEndpoint: "http://localhost:4318", // Default OTLP/HTTP endpoint
	}

	shutdown, err := o11y.Init(ctx, cfg)
	if err != nil {
		slog.Error("Failed to initialize o11y SDK", slog.Any("error", err))
		return
	}

	// Ensure the TracerProvider is shut down properly at the end
	defer shutdown(ctx)

	slog.Info("SDK initialized successfully")

	// 2. Start a root span
	tracer := otel.Tracer("example-tracer")
	ctx, rootSpan := tracer.Start(ctx, "root-operation")
	defer rootSpan.End()

	// 3. Log a message within the context of the span
	// The OtelSlogHandler will automatically extract trace_id and span_id
	slog.InfoContext(ctx, "Processing root operation")
	time.Sleep(100 * time.Millisecond)

	// 4. Simulate some work and a child span
	performChildOperation(ctx)

	slog.InfoContext(ctx, "Example completed")
}

func performChildOperation(ctx context.Context) {
	tracer := otel.Tracer("example-tracer")
	ctx, childSpan := tracer.Start(ctx, "child-operation")
	defer childSpan.End()

	slog.InfoContext(ctx, "Performing child operation")
	time.Sleep(100 * time.Millisecond)
	slog.InfoContext(ctx, "Child operation finished")
}
