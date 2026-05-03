// Package main is the backend subscriber for the nats-ws-browser example.
// It receives NATS Core messages published by the browser frontend,
// processes them within a child span linked to the browser's trace,
// and publishes a reply so the browser can see the full round-trip in Tempo.
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
	pubSubject = "demo.frontend.events"
	repSubject = "demo.frontend.replies"
	natsURL    = nats.DefaultURL
)

// metricsAddr allows the Prometheus scrape port to be overridden via
// METRICS_ADDR so this process can run alongside other example services
// on the same machine without port conflicts.
func metricsAddr() string {
	if v := os.Getenv("METRICS_ADDR"); v != "" {
		return v
	}
	return ":2114"
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	obs, err := o11y.Init(ctx,
		o11y.WithServiceName("nats-ws-browser-backend"),
		o11y.WithServiceVersion("0.1.0"),
		o11y.WithEnvironment("development"),
		o11y.WithServiceNamespace("platform"),
		o11y.WithMetricsAddr(metricsAddr()),
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
	tracer := obs.Tracer("nats-ws-browser-backend")

	conn, err := o11ynats.Connect(ctx, natsURL, obs.TracerProvider(), obs.Propagator)
	if err != nil {
		logger.ErrorContext(ctx, "failed to connect to NATS", slog.Any("error", err))
		os.Exit(1)
	}
	defer conn.Drain() //nolint:errcheck

	logger.InfoContext(ctx, "connected to NATS", slog.String("url", natsURL))

	_, err = conn.Subscribe(ctx, pubSubject, func(msgCtx context.Context, msg *nats.Msg) {
		// msgCtx already carries the consumer span created by otel-nats, which
		// extracted the browser's traceparent from the message headers.
		// Starting a child span here links this backend work to the browser trace.
		msgCtx, span := tracer.Start(msgCtx, "process-frontend-event")
		defer span.End()

		payload := string(msg.Data)
		logger.InfoContext(msgCtx, "received from browser",
			slog.String("subject", msg.Subject),
			slog.String("payload", payload),
		)

		// Reply via conn.Publish so trace context is injected into the reply
		// headers — the browser subscriber will extract it and close the loop.
		reply := "backend processed: " + payload
		if err := conn.Publish(msgCtx, repSubject, []byte(reply)); err != nil {
			logger.ErrorContext(msgCtx, "reply failed", slog.Any("error", err))
		}
	})
	if err != nil {
		logger.ErrorContext(ctx, "subscribe failed", slog.Any("error", err))
		os.Exit(1)
	}

	logger.InfoContext(ctx, "backend subscriber ready",
		slog.String("listening", pubSubject),
		slog.String("replying", repSubject),
	)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.InfoContext(ctx, "shutting down backend subscriber")
}
