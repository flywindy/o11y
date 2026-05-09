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
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	pubSubject = "demo.frontend.events"
	repSubject = "demo.frontend.replies"
	natsURL    = nats.DefaultURL
)

type natsHeaderCarrier nats.Header

func (c natsHeaderCarrier) Get(key string) string {
	return nats.Header(c).Get(key)
}

func (c natsHeaderCarrier) Set(key, value string) {
	if c == nil {
		return
	}
	nats.Header(c).Set(key, value)
}

func (c natsHeaderCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}

// metricsAddr returns the Prometheus scrape address, overridable via METRICS_ADDR.
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
		slog.ErrorContext(ctx, "failed to initialise o11y SDK", slog.Any("error", err))
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := obs.Shutdown(shutdownCtx); err != nil {
			slog.ErrorContext(shutdownCtx, "SDK shutdown error", slog.Any("error", err))
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

	_, err = conn.NatsConn().Subscribe(pubSubject, func(msg *nats.Msg) {
		// The SDK's NATS wrapper follows OTel messaging semantics and correlates
		// consumer spans with links. This browser round-trip demo wants a single
		// Tempo trace tree, so it extracts the browser traceparent directly and
		// starts the backend work as a child span.
		msgCtx := obs.Propagator.Extract(context.Background(), natsHeaderCarrier(msg.Header))
		msgCtx, span := tracer.Start(msgCtx, "process-frontend-event")
		defer span.End()

		payload := string(msg.Data)
		logger.InfoContext(msgCtx, "received from browser",
			slog.String("subject", msg.Subject),
			slog.String("payload", payload),
		)

		// Reply with explicit header injection so this demo does not depend on
		// otel-nats env flags such as OTEL_NATS_TRACING_ENABLED.
		reply := "backend processed: " + payload
		replyCtx, replySpan := tracer.Start(msgCtx, "send "+repSubject,
			trace.WithSpanKind(trace.SpanKindProducer),
			trace.WithAttributes(
				semconv.MessagingSystemKey.String("nats"),
				semconv.MessagingDestinationNameKey.String(repSubject),
				attribute.String(string(semconv.MessagingOperationTypeKey), "publish"),
				semconv.MessagingOperationNameKey.String("publish"),
			),
		)
		defer replySpan.End()

		replyMsg := &nats.Msg{
			Subject: repSubject,
			Data:    []byte(reply),
			Header:  nats.Header{},
		}
		obs.Propagator.Inject(replyCtx, natsHeaderCarrier(replyMsg.Header))

		if err := conn.NatsConn().PublishMsg(replyMsg); err != nil {
			replySpan.RecordError(err)
			replySpan.SetStatus(codes.Error, err.Error())
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
