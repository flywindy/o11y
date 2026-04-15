// Package main demonstrates a JetStream pull consumer instrumented with the o11y SDK.
// The subscriber uses a durable consumer with Consume (push-style pull), which keeps
// the goroutine alive and processes messages as they arrive.
// Run together with examples/jetstream/publisher to see end-to-end trace correlation.
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
	streamName   = "EVENTS"
	subject      = "events.created"
	consumerName = "events-processor"
	natsURL      = nats.DefaultURL
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Initialise the o11y SDK.
	obs, err := o11y.Init(ctx,
		o11y.WithServiceName("jetstream-subscriber"),
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
	conn, err := o11ynats.Connect(ctx, natsURL, obs.TracerProvider(), obs.Propagator)
	if err != nil {
		logger.ErrorContext(ctx, "failed to connect to NATS", slog.Any("error", err))
		os.Exit(1)
	}
	defer conn.Drain() //nolint:errcheck

	// 3. Obtain a tracing-aware JetStream interface.
	js, err := conn.JetStream()
	if err != nil {
		logger.ErrorContext(ctx, "failed to create JetStream context", slog.Any("error", err))
		os.Exit(1)
	}

	// 4. Look up the stream. The publisher is responsible for stream creation;
	//    the subscriber only needs a reference to it.
	stream, err := js.Stream(ctx, streamName)
	if err != nil {
		logger.ErrorContext(ctx, "stream not found — start the publisher first",
			slog.String("stream", streamName),
			slog.Any("error", err),
		)
		os.Exit(1)
	}

	// 5. Create or update a durable consumer. Using CreateOrUpdateConsumer makes
	//    the subscriber idempotent: restarting the process resumes from where it
	//    left off rather than reprocessing already-acknowledged messages.
	consumer, err := stream.CreateOrUpdateConsumer(ctx, oteljetstream.ConsumerConfig{
		Durable:       consumerName,
		FilterSubject: subject,
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	if err != nil {
		logger.ErrorContext(ctx, "failed to create consumer", slog.Any("error", err))
		os.Exit(1)
	}

	tracer := obs.Tracer("jetstream-subscriber")

	// 6. Consume messages. oteljetstream extracts the publisher's trace context
	//    from each message's headers and links it to a new consumer span.
	//    m.Context() carries that span context so slog calls will include the
	//    correct trace_id and span_id.
	cc, err := consumer.Consume(func(m oteljetstream.Msg) {
		msgCtx, span := tracer.Start(m.Context(), "process-event")
		defer span.End()

		logAttrs := []any{
			slog.String("subject", m.Subject()),
			slog.String("payload", string(m.Data())),
		}
		if meta, err := m.Metadata(); err == nil {
			logAttrs = append(logAttrs,
				slog.String("stream", meta.Stream),
				slog.Uint64("sequence", meta.Sequence.Stream),
			)
		}
		logger.InfoContext(msgCtx, "JetStream event received", logAttrs...)

		// Acknowledge the message so JetStream does not redeliver it.
		if err := m.Ack(); err != nil {
			logger.ErrorContext(msgCtx, "ack failed", slog.Any("error", err))
		}
	})
	if err != nil {
		logger.ErrorContext(ctx, "consume failed", slog.Any("error", err))
		os.Exit(1)
	}
	defer cc.Stop()

	logger.InfoContext(ctx, "subscriber ready",
		slog.String("stream", streamName),
		slog.String("consumer", consumerName),
	)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.InfoContext(ctx, "shutting down subscriber")
}
