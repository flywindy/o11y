// Package main demonstrates a NATS Core subscriber instrumented with the o11y SDK.
// Run together with examples/nats-core/publisher to see distributed trace correlation.
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
	subject = "o11y.events"
	natsURL = nats.DefaultURL
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Initialise the o11y SDK.
	obs, err := o11y.Init(ctx,
		o11y.WithServiceName("nats-core-subscriber"),
		o11y.WithServiceVersion("0.1.0"),
		o11y.WithEnvironment("development"),
		o11y.WithServiceNamespace("platform"),
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
	conn, err := o11ynats.Connect(ctx, natsURL, obs.TracerProvider(), obs.Propagator)
	if err != nil {
		logger.ErrorContext(ctx, "failed to connect to NATS", slog.Any("error", err))
		os.Exit(1)
	}
	defer conn.Drain() //nolint:errcheck

	logger.InfoContext(ctx, "connected to NATS", slog.String("url", natsURL))

	tracer := obs.Tracer("nats-core-subscriber")

	// 3. Subscribe. The MsgHandler receives a ctx carrying a consumer span
	//    created by the otelnats layer. That consumer span holds a span link to
	//    the publisher's trace, enabling cross-service correlation in Tempo.
	//    Any span started from ctx is a child of the consumer span, and any
	//    slog call with ctx will include the correct traceId and spanId.
	_, err = conn.Subscribe(subject, func(msgCtx context.Context, msg *nats.Msg) {
		msgCtx, span := tracer.Start(msgCtx, "process-event")
		defer span.End()

		logger.InfoContext(msgCtx, "event received",
			slog.String("subject", msg.Subject),
			slog.String("payload", string(msg.Data)),
		)

		// To reply while preserving trace context use conn.Publish, not msg.Respond.
		// msg.Respond(data) routes through the raw NATS connection and does not
		// inject trace headers into the reply.
	})
	if err != nil {
		logger.ErrorContext(ctx, "subscribe failed", slog.Any("error", err))
		os.Exit(1)
	}

	logger.InfoContext(ctx, "subscriber ready", slog.String("subject", subject))

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.InfoContext(ctx, "shutting down subscriber")
}
