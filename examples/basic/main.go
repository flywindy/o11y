package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/flywindy/o11y"
)

func main() {
	ctx := context.Background()

	// 1. Initialize the SDK (no global state mutated)
	sdk, err := o11y.Init(ctx,
		o11y.WithServiceName("basic-example"),
		o11y.WithServiceVersion("0.1.0"),
		o11y.WithEnvironment("development"),
		o11y.WithOTLPEndpoint("http://localhost:4318"),
		o11y.WithLogLevel(slog.LevelInfo),
	)
	if err != nil {
		slog.ErrorContext(ctx, "failed to initialize o11y SDK", slog.Any("error", err))
		return
	}

	// 2. Flush in-flight spans on exit.
	//    A dedicated context with a timeout ensures the shutdown completes promptly.
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := sdk.Shutdown(shutdownCtx); err != nil {
			sdk.Logger.ErrorContext(shutdownCtx, "SDK shutdown error", slog.Any("error", err))
		}
	}()

	sdk.Logger.Info("SDK initialized successfully")

	// 3. Start a root span using the SDK's TracerProvider (no global OTel state needed)
	tracer := sdk.Tracer("example-tracer")
	ctx, rootSpan := tracer.Start(ctx, "root-operation")
	defer rootSpan.End()

	sdk.Logger.InfoContext(ctx, "processing root operation")
	time.Sleep(100 * time.Millisecond)

	// 4. Child span
	performChildOperation(ctx, sdk)

	sdk.Logger.InfoContext(ctx, "example completed")
}

func performChildOperation(ctx context.Context, sdk *o11y.SDK) {
	tracer := sdk.Tracer("example-tracer")
	ctx, span := tracer.Start(ctx, "child-operation")
	defer span.End()

	sdk.Logger.InfoContext(ctx, "performing child operation")
	time.Sleep(100 * time.Millisecond)
	sdk.Logger.InfoContext(ctx, "child operation finished")
}
