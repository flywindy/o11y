package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"github.com/flywindy/o11y"
	o11ynats "github.com/flywindy/o11y/nats"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// 1. Initialize o11y SDK for the publisher
	shutdown, err := o11y.Init(ctx,
		o11y.WithServiceName("nats-publisher"),
		o11y.WithEnvironment("development"),
	)
	if err != nil {
		slog.Error("Failed to initialize o11y SDK", slog.Any("error", err))
		os.Exit(1)
	}
	defer shutdown(ctx)

	// Debug check: Confirm the SDK actually initialized the W3C propagator
	slog.Info("Propagator check", slog.Any("type", fmt.Sprintf("%T", otel.GetTextMapPropagator())))

	// 2. Connect to NATS
	conn, err := o11ynats.NewTracedConn(nats.DefaultURL, nil)
	if err != nil {
		slog.Error("Failed to connect to NATS", slog.Any("error", err))
		return
	}
	defer conn.Close()

	slog.Info("Publisher connected to NATS")

	// 3. Publish loop
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("Publisher shutting down...")
			return
		case t := <-ticker.C:
			publish(ctx, conn, t)
		}
	}
}

func publish(ctx context.Context, conn *o11ynats.Conn, t time.Time) {
	// Start a manual span for the publishing operation
	tracer := otel.Tracer("nats-publisher")
	childCtx, span := tracer.Start(ctx, "publish-message")
	defer span.End()

	payload := "Message sent at " + t.Format(time.RFC3339)
	slog.InfoContext(childCtx, "Preparing to publish message", slog.String("payload", payload))

	// The trace context in 'ctx' is automatically injected into NATS headers
	err := conn.Publish(childCtx, "telemetry.demo", []byte(payload))
	if err != nil {
		slog.ErrorContext(childCtx, "Failed to publish", slog.Any("error", err))
	}
}
