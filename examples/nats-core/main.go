package main

import (
	"context"
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
	// 1. Initialize o11y SDK
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	shutdown, err := o11y.Init(ctx,
		o11y.WithServiceName("nats-example"),
		o11y.WithEnvironment("development"),
	)
	if err != nil {
		slog.Error("Failed to initialize o11y SDK", slog.Any("error", err))
		os.Exit(1)
	}
	defer shutdown(ctx)

	// 2. Connect to NATS using the simplified factory
	// Note: Only o11ynats and nats-io are imported
	conn, err := o11ynats.NewTracedConn(nats.DefaultURL, nil)
	if err != nil {
		slog.Error("Failed to connect to NATS", slog.Any("error", err))
		return
	}
	defer conn.Close()

	slog.Info("Connected to NATS via o11y SDK")

	// 3. Implement a Subscriber with the simplified (ctx, msg) signature
	subject := "telemetry.demo"
	_, err = conn.Subscribe(subject, func(ctx context.Context, msg *nats.Msg) {
		// Log message with context to demonstrate trace correlation
		slog.InfoContext(ctx, "Subscriber received message",
			slog.String("subject", msg.Subject),
			slog.String("data", string(msg.Data)),
		)
	})
	if err != nil {
		slog.Error("Failed to subscribe", slog.Any("error", err))
		return
	}

	// 4. Implement a Publisher goroutine
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				publishMessage(conn)
			}
		}
	}()

	slog.Info("Service running. Press Ctrl+C to stop.")

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)
	<-sigChan
	slog.Info("Shutting down...")
}

func publishMessage(conn *o11ynats.Conn) {
	// Start a manual span in the producer
	tracer := otel.Tracer("nats-example")
	ctx, span := tracer.Start(context.Background(), "manual-trigger")
	defer span.End()

	payload := "Data generated at " + time.Now().Format(time.Kitchen)
	slog.InfoContext(ctx, "Publisher sending message", slog.String("payload", payload))

	// Publish message using the context for automatic trace propagation.
	// conn.Publish is available via embedding.
	err := conn.Publish(ctx, "telemetry.demo", []byte(payload))
	if err != nil {
		slog.ErrorContext(ctx, "Publisher error", slog.Any("error", err))
	}
}
