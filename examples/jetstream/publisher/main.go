// Package main demonstrates a JetStream publisher instrumented with the o11y SDK.
// The publisher creates the stream if it does not exist, then publishes one
// message every 3 seconds with full OTel trace context injected into headers.
// Run together with examples/jetstream/subscriber to see end-to-end trace correlation.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Marz32onE/instrumentation-go/otel-nats/oteljetstream"
	"github.com/flywindy/o11y"
	o11ynats "github.com/flywindy/o11y/nats"
	"github.com/nats-io/nats.go"
)

const (
	streamName  = "EVENTS"
	subject     = "events.created"
	natsURL     = nats.DefaultURL
	publishRate = 3 * time.Second
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Initialise the o11y SDK.
	sdk, err := o11y.Init(ctx,
		o11y.WithServiceName("jetstream-publisher"),
		o11y.WithEnvironment("development"),
	)
	if err != nil {
		slog.Error("failed to initialise o11y SDK", slog.Any("error", err))
		os.Exit(1)
	}
	defer func() {
		if err := sdk.Shutdown(ctx); err != nil {
			slog.Error("SDK shutdown error", slog.Any("error", err))
		}
	}()

	logger := sdk.Logger

	// 2. Connect to NATS with trace instrumentation wired from the SDK.
	conn, err := o11ynats.Connect(natsURL, sdk.TracerProvider(), sdk.Propagator)
	if err != nil {
		logger.ErrorContext(ctx, "failed to connect to NATS", slog.Any("error", err))
		os.Exit(1)
	}
	defer conn.Drain() //nolint:errcheck

	// 3. Obtain a tracing-aware JetStream interface. The JetStream layer
	//    inherits the TracerProvider and Propagator from conn.
	js, err := conn.JetStream()
	if err != nil {
		logger.ErrorContext(ctx, "failed to create JetStream context", slog.Any("error", err))
		os.Exit(1)
	}

	// 4. Create or update the stream. CreateOrUpdateStream is idempotent, so
	//    it is safe to call on every startup without checking whether the
	//    stream already exists.
	_, err = js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{subject},
	})
	if err != nil {
		logger.ErrorContext(ctx, "failed to create stream", slog.Any("error", err))
		os.Exit(1)
	}

	logger.InfoContext(ctx, "stream ready",
		slog.String("stream", streamName),
		slog.String("subject", subject),
	)

	tracer := sdk.Tracer("jetstream-publisher")

	ticker := time.NewTicker(publishRate)
	defer ticker.Stop()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case <-quit:
			logger.InfoContext(ctx, "shutting down publisher")
			return

		case t := <-ticker.C:
			// Wrap each publish in its own root span so Grafana Tempo shows a
			// clean producer → broker → consumer trace across services.
			pubCtx, span := tracer.Start(ctx, "publish-event")

			payload := []byte("event at " + t.Format(time.RFC3339))

			// js.Publish injects the active span's trace context into the
			// JetStream message headers automatically.
			ack, err := js.Publish(pubCtx, subject, payload)
			if err != nil {
				logger.ErrorContext(pubCtx, "JetStream publish failed", slog.Any("error", err))
			} else {
				logger.InfoContext(pubCtx, "event published to JetStream",
					slog.String("subject", subject),
					slog.String("stream", ack.Stream),
					slog.Uint64("sequence", ack.Sequence),
					slog.String("payload", string(payload)),
				)
			}

			span.End()
		}
	}
}
