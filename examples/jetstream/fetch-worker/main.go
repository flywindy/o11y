// Package main demonstrates a JetStream batch-pull worker instrumented with
// the o11y SDK, using Consumer.Fetch instead of the push-style Consume shown
// in examples/jetstream/subscriber. Fetch/FetchBytes/FetchNoWait suit workers
// that want to pull and process a bounded batch of messages at a time (e.g. a
// search-index sync worker bulk-upserting a batch per round trip) rather than
// react to one message per callback invocation.
// Run together with examples/jetstream/publisher to see end-to-end trace
// correlation; it can also run alongside examples/jetstream/subscriber against
// the same stream — each uses its own durable consumer, so JetStream delivers
// every message to both independently.
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
	"github.com/nats-io/nats.go/jetstream"
)

const (
	streamName   = "EVENTS"
	subject      = "events.created"
	consumerName = "events-fetch-worker"
	natsURL      = nats.DefaultURL
	batchSize    = 5
	fetchMaxWait = 3 * time.Second
	// fetchErrorBackoff bounds the retry rate after a non-cancellation Fetch
	// error, so a persistent failure (consumer deleted, auth failure, ...)
	// doesn't spin the loop at full CPU logging the same error forever.
	fetchErrorBackoff = 1 * time.Second
)

func metricsAddr(ctx context.Context) string {
	_ = ctx
	if v := os.Getenv("METRICS_ADDR"); v != "" {
		return v
	}
	return ":2117"
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Initialise the o11y SDK.
	obs, err := o11y.Init(ctx,
		o11y.WithServiceName("jetstream-fetch-worker"),
		o11y.WithServiceVersion("0.1.0"),
		o11y.WithEnvironment("development"),
		o11y.WithServiceNamespace("platform"),
		o11y.WithMetricsAddr(metricsAddr(ctx)),
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
	//    this worker only needs a reference to it.
	stream, err := js.Stream(ctx, streamName)
	if err != nil {
		logger.ErrorContext(ctx, "stream not found — start the publisher first",
			slog.String("stream", streamName),
			slog.Any("error", err),
		)
		os.Exit(1)
	}

	// 5. Create or update a durable consumer distinct from the subscriber
	//    example's, so both can run against the same stream independently.
	consumer, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       consumerName,
		FilterSubject: subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		logger.ErrorContext(ctx, "failed to create consumer", slog.Any("error", err))
		os.Exit(1)
	}

	tracer := obs.Tracer("jetstream-fetch-worker")

	logger.InfoContext(ctx, "fetch worker ready",
		slog.String("stream", streamName),
		slog.String("consumer", consumerName),
		slog.Int("batch_size", batchSize),
	)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case <-quit:
			logger.InfoContext(ctx, "shutting down fetch worker")
			return
		default:
		}

		// 6. Pull up to batchSize messages in one round trip. jetstream.FetchMaxWait
		//    bounds how long the request stays open waiting for a full batch —
		//    without it, a mostly-idle stream would leave the request (and this
		//    loop iteration) open for the library's default wait on every pass.
		batch, err := consumer.Fetch(ctx, batchSize, jetstream.FetchMaxWait(fetchMaxWait))
		if err != nil {
			// A cancelled ctx makes Fetch fail immediately (not after
			// fetchMaxWait), so without this check a shutdown signal arriving
			// here (rather than being caught by the <-quit case above) would
			// spin the loop at full CPU logging the same error forever.
			if ctx.Err() != nil {
				logger.InfoContext(ctx, "shutting down fetch worker")
				return
			}
			// A persistent non-cancellation error (consumer deleted, auth
			// failure, ...) would otherwise retry immediately forever,
			// spinning the loop at full CPU and flooding the logs. ctx is
			// only cancelled by main's deferred cancel() on return, so <-quit
			// (not <-ctx.Done()) is what makes this backoff exit promptly on
			// a SIGINT/SIGTERM that arrives mid-sleep.
			logger.ErrorContext(ctx, "fetch failed", slog.Any("error", err))
			select {
			case <-time.After(fetchErrorBackoff):
			case <-quit:
				logger.InfoContext(ctx, "shutting down fetch worker")
				return
			}
			continue
		}

		// Each FetchedMessage pairs the native jetstream.Msg with the
		// consumer-span ctx extracted from that message's headers — same
		// (ctx, msg) shape as Consume/Messages, just delivered over a channel.
		var processed int
		for m := range batch.Messages() {
			msgCtx, span := tracer.Start(m.Ctx, "process-event-batch")

			logAttrs := []any{
				slog.String("subject", m.Msg.Subject()),
				slog.String("payload", string(m.Msg.Data())),
			}
			if meta, err := m.Msg.Metadata(); err == nil {
				logAttrs = append(logAttrs,
					slog.String("stream", meta.Stream),
					slog.Uint64("sequence", meta.Sequence.Stream),
				)
			}
			logger.InfoContext(msgCtx, "JetStream event received (batch)", logAttrs...)

			if err := m.Msg.Ack(); err != nil {
				logger.ErrorContext(msgCtx, "ack failed", slog.Any("error", err))
			}
			processed++
			span.End()
		}
		if err := batch.Error(); err != nil {
			logger.ErrorContext(ctx, "batch completed with error", slog.Any("error", err))
		}
		if processed > 0 {
			logger.InfoContext(ctx, "batch processed", slog.Int("count", processed))
		}
	}
}
