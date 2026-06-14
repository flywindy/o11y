// Package main demonstrates a NATS Core request/reply requester instrumented
// with the o11y SDK. It sends a request every requestRate inside its own root
// span and logs the reply.
// Run together with examples/nats-core/responder to verify that replies sent by
// conn.Respond carry trace headers. conn.Request does not yet extract those
// reply headers or create a requester-side receive span.
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
	subject        = "o11y.rpc.greet"
	natsURL        = nats.DefaultURL
	requestRate    = 3 * time.Second
	requestTimeout = 2 * time.Second
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Initialise the o11y SDK.
	obs, err := o11y.Init(ctx,
		o11y.WithServiceName("nats-core-requester"),
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

	tracer := obs.Tracer("nats-core-requester")

	ticker := time.NewTicker(requestRate)
	defer ticker.Stop()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case <-quit:
			logger.InfoContext(ctx, "shutting down requester")
			return

		case <-ticker.C:
			// Each request lives inside its own root span. conn.Request injects
			// the active trace context into the request headers; the responder's
			// reply (sent via conn.Respond) carries trace headers back, but this
			// requester does not extract them into a receive span yet.
			reqCtx, span := tracer.Start(ctx, "send-request")

			reply, err := conn.Request(reqCtx, subject, []byte("o11y"), requestTimeout)
			if err != nil {
				logger.ErrorContext(reqCtx, "request failed", slog.Any("error", err))
			} else {
				logger.InfoContext(reqCtx, "reply received",
					slog.String("subject", subject),
					slog.String("reply", string(reply.Data)),
				)
			}

			span.End()
		}
	}
}
