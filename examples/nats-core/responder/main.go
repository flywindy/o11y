// Package main demonstrates a NATS Core request/reply responder instrumented
// with the o11y SDK. It replies with conn.Respond, which routes the reply
// through the traced publish path so the response carries trace context.
// Raw msg.Respond skips header injection and makes the reply untraced.
// Run together with examples/nats-core/requester to verify reply propagation.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/flywindy/o11y"
	o11ynats "github.com/flywindy/o11y/nats"
	"github.com/nats-io/nats.go"
)

const (
	subject = "o11y.rpc.greet"
	natsURL = nats.DefaultURL
)

func metricsAddr() string {
	if v := os.Getenv("METRICS_ADDR"); v != "" {
		return v
	}
	return ":2116"
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Initialise the o11y SDK.
	obs, err := o11y.Init(ctx,
		o11y.WithServiceName("nats-core-responder"),
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

	// 2. Connect to NATS with trace instrumentation wired from the SDK.
	conn, err := o11ynats.Connect(ctx, natsURL, obs.TracerProvider(), obs.Propagator)
	if err != nil {
		logger.ErrorContext(ctx, "failed to connect to NATS", slog.Any("error", err))
		os.Exit(1)
	}
	defer conn.Drain() //nolint:errcheck

	logger.InfoContext(ctx, "connected to NATS", slog.String("url", natsURL))

	tracer := obs.Tracer("nats-core-responder")

	// 3. Subscribe and reply with conn.Respond. The handler's ctx already holds a
	//    consumer span linked to the requester's trace; Respond routes the reply
	//    through the traced publish path, injecting that context into the reply
	//    headers. Using raw msg.Respond here would drop the reply trace headers.
	_, err = conn.Subscribe(ctx, subject, func(msgCtx context.Context, msg *nats.Msg) {
		msgCtx, span := tracer.Start(msgCtx, "handle-greet")
		defer span.End()

		name := strings.TrimSpace(string(msg.Data))
		if name == "" {
			name = "world"
		}

		logger.InfoContext(msgCtx, "request received",
			slog.String("subject", msg.Subject),
			slog.String("name", name),
		)

		if err := conn.Respond(msgCtx, msg, []byte("hello, "+name)); err != nil {
			logger.ErrorContext(msgCtx, "respond failed", slog.Any("error", err))
		}
	})
	if err != nil {
		logger.ErrorContext(ctx, "subscribe failed", slog.Any("error", err))
		os.Exit(1)
	}

	logger.InfoContext(ctx, "responder ready", slog.String("subject", subject))

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.InfoContext(ctx, "shutting down responder")
}
