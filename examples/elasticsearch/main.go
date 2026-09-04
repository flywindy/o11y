// Package main demonstrates Elasticsearch tracing through the o11y
// elasticsearch wrapper, which wires the SDK TracerProvider into
// go-elasticsearch's first-party OpenTelemetry instrumentation.
//
// The example points at a local Elasticsearch
// (k8s/infrastructure/base/elasticsearch.yaml for the cluster spec, or
// `docker run -p 9200:9200 -e discovery.type=single-node \
// -e xpack.security.enabled=false \
// docker.elastic.co/elasticsearch/elasticsearch:8.19.3`). It loops through
// Index → Search every few seconds so each request appears as a
// SpanKindClient span carrying the upstream's db.system / db.operation /
// db.elasticsearch.path_parts.* attributes and records one
// db.client.operation.duration sample labeled by operation and index.
// WithSearchBody(true) additionally records the search query body as
// db.statement.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	elastic "github.com/elastic/go-elasticsearch/v8"
	"github.com/flywindy/o11y"
	o11yes "github.com/flywindy/o11y/elasticsearch"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	index       = "o11y-demo"
	docID       = "1"
	defaultAddr = "http://localhost:9200"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		slog.ErrorContext(context.Background(), "Elasticsearch example failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	obs, err := o11y.Init(ctx,
		o11y.WithServiceName("elasticsearch-example"),
		o11y.WithServiceVersion("0.1.0"),
		o11y.WithEnvironment("development"),
		o11y.WithServiceNamespace("platform"),
		o11y.WithOTLPEndpoint("http://localhost:4318"),
		o11y.WithMetricsOTLPEndpoint("http://localhost:4318"),
		o11y.WithLogLevel(slog.LevelInfo),
	)
	if err != nil {
		return fmt.Errorf("initialize o11y SDK: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := obs.Shutdown(shutdownCtx); err != nil {
			obs.Logger.ErrorContext(shutdownCtx, "SDK shutdown error", slog.Any("error", err))
		}
	}()

	cfg := elastic.Config{
		Addresses: []string{envOr("ELASTICSEARCH_URL", defaultAddr)},
		Username:  os.Getenv("ELASTICSEARCH_USERNAME"),
		Password:  os.Getenv("ELASTICSEARCH_PASSWORD"),
	}

	// Spans come from the client's first-party instrumentation on the SDK
	// TracerProvider; the SDK-owned db.client.operation.duration histogram is
	// recorded on the SDK MeterProvider (ADR 0027). WithSearchBody(true) opts
	// into recording search query bodies as db.statement.
	client, err := o11yes.NewClient(cfg, obs.TracerProvider(), obs.MeterProvider(), o11yes.WithSearchBody(true))
	if err != nil {
		return fmt.Errorf("construct Elasticsearch client: %w", err)
	}

	tracer := obs.Tracer("elasticsearch-example")
	logger := obs.Logger
	logger.InfoContext(ctx, "Elasticsearch example started",
		slog.String("addresses", strings.Join(cfg.Addresses, ",")),
		slog.String("index", index),
	)

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		runCycle(ctx, logger, tracer, client)
		select {
		case <-ctx.Done():
			logger.InfoContext(ctx, "Elasticsearch example stopped")
			return nil
		case <-ticker.C:
		}
	}
}

func runCycle(
	ctx context.Context,
	logger *slog.Logger,
	tracer trace.Tracer,
	client *elastic.Client,
) {
	cycleCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cycleCtx, span := tracer.Start(cycleCtx, "elasticsearch-cycle")
	defer span.End()

	// Index — refresh=true so the document is immediately searchable. The
	// index is created on first write by Elasticsearch's default
	// action.auto_create_index; no explicit create step is needed (unlike S3
	// buckets in the minio example).
	doc := fmt.Sprintf(`{"title":"hello","ts":%q}`, time.Now().UTC().Format(time.RFC3339))
	res, err := client.Index(index, strings.NewReader(doc),
		client.Index.WithDocumentID(docID),
		client.Index.WithRefresh("true"),
		client.Index.WithContext(cycleCtx),
	)
	if err != nil {
		failCycle(cycleCtx, logger, span, "Index", err)
		return
	}
	if res == nil {
		failCycle(cycleCtx, logger, span, "Index", errors.New("nil response received"))
		return
	}
	drain(res.Body)
	if res.IsError() {
		failCycle(cycleCtx, logger, span, "Index", fmt.Errorf("status %s", res.Status()))
		return
	}

	// Search — the query body is captured as db.statement because the client
	// was built with WithSearchBody(true).
	res, err = client.Search(
		client.Search.WithContext(cycleCtx),
		client.Search.WithIndex(index),
		client.Search.WithBody(strings.NewReader(`{"query":{"match_all":{}}}`)),
	)
	if err != nil {
		failCycle(cycleCtx, logger, span, "Search", err)
		return
	}
	if res == nil {
		failCycle(cycleCtx, logger, span, "Search", errors.New("nil response received"))
		return
	}
	drain(res.Body)
	if res.IsError() {
		failCycle(cycleCtx, logger, span, "Search", fmt.Errorf("status %s", res.Status()))
		return
	}

	logger.InfoContext(cycleCtx, "cycle complete", slog.String("index", index))
}

func failCycle(ctx context.Context, logger *slog.Logger, span trace.Span, step string, err error) {
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	logger.ErrorContext(ctx, step+" failed", slog.Any("error", err))
}

// drain reads and closes an esapi response body so the connection can be
// reused by the keep-alive pool.
func drain(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, body)
	_ = body.Close()
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
