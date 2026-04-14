// Package main demonstrates a NATS Core publisher instrumented with the o11y SDK.
// Run together with examples/nats-core/subscriber to see distributed trace correlation.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/flywindy/o11y"
	o11ynats "github.com/flywindy/o11y/nats"
	"github.com/nats-io/nats.go"
)

const (
	subject     = "o11y.events"
	natsURL     = nats.DefaultURL
	publishRate = 3 * time.Second
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Initialise the o11y SDK. TracerProvider and Propagator live on the
	//    returned SDK struct — no global OTel state is modified.
	obs, err := o11y.Init(ctx,
		o11y.WithServiceName("nats-core-publisher"),
		o11y.WithEnvironment("development"),
	)
	if err != nil {
		slog.Error("failed to initialise o11y SDK", slog.Any("error", err))
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := obs.Shutdown(shutdownCtx); err != nil {
			slog.Error("SDK shutdown error", slog.Any("error", err))
		}
	}()

	logger := obs.Logger

	// 2. Connect to NATS with trace instrumentation wired from the SDK.
	//    No global otel state is used; TracerProvider and Propagator come
	//    directly from the SDK struct.
	conn, err := o11ynats.Connect(ctx, natsURL, obs.TracerProvider(), obs.Propagator)
	if err != nil {
		logger.ErrorContext(ctx, "failed to connect to NATS", slog.Any("error", err))
		os.Exit(1)
	}
	defer conn.Drain() //nolint:errcheck

	logger.InfoContext(ctx, "connected to NATS", slog.String("url", natsURL))

	// 3. Publish a message every publishRate, each inside its own root span.
	ticker := time.NewTicker(publishRate)
	defer ticker.Stop()

	tracer := obs.Tracer("nats-core-publisher")

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case <-quit:
			logger.InfoContext(ctx, "shutting down publisher")
			return

		case t := <-ticker.C:
			// Each publish lives inside its own root span so Grafana Tempo
			// shows a clear producer → consumer trace across services.
			pubCtx, span := tracer.Start(ctx, "publish-event")

			payload := []byte("event at " + t.Format(time.RFC3339))
			if err := conn.Publish(pubCtx, subject, payload); err != nil {
				logger.ErrorContext(pubCtx, "publish failed", slog.Any("error", err))
			} else {
				logger.InfoContext(pubCtx, "event published",
					slog.String("subject", subject),
					slog.String("payload", string(payload)),
				)
			}

			span.End()
		}
	}
}
