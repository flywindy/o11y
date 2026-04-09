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

	// 1. Initialize o11y SDK for the subscriber
	shutdown, err := o11y.Init(ctx,
		o11y.WithServiceName("nats-subscriber"),
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

	slog.Info("Subscriber connected to NATS")

	// 3. Subscribe with our (ctx, msg) signature
	subject := "telemetry.demo"
	_, err = conn.Subscribe(subject, func(ctx context.Context, msg *nats.Msg) {
		// Use the context provided by our SDK (extracted from the library)
		// This context now contains the Consumer Span linked to the Producer Span.
		tracer := otel.Tracer("nats-subscriber")
		childCtx, span := tracer.Start(ctx, "process-message")
		defer span.End()

		slog.InfoContext(childCtx, "Subscriber processing message",
			slog.String("subject", msg.Subject),
			slog.String("payload", string(msg.Data)),
		)

		// Simulate some processing time
		time.Sleep(100 * time.Millisecond)
	})
	if err != nil {
		slog.Error("Failed to subscribe", slog.Any("error", err))
		return
	}

	slog.Info("Subscriber listening... Press Ctrl+C to exit.")

	// 4. Wait for signal
	<-ctx.Done()
	slog.Info("Subscriber shutting down...")
}
