// Package main demonstrates Cassandra tracing and metrics through the o11y
// Cassandra integration (ADR 0019).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/flywindy/o11y"
	o11ycassandra "github.com/flywindy/o11y/cassandra"
	"github.com/gocql/gocql"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	defaultCassandraAddr = "localhost:9042"
	keyspace             = "o11y_examples"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		slog.ErrorContext(context.Background(), "Cassandra example failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	obs, err := o11y.Init(ctx,
		o11y.WithServiceName("cassandra-example"),
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

	addr := envOrDefault("CASSANDRA_ADDR", defaultCassandraAddr)

	// The caller keeps full control of the cluster config (contact points,
	// consistency, auth, timeouts); NewSession only attaches the observers.
	cluster := gocql.NewCluster(addr)
	cluster.Consistency = gocql.LocalQuorum
	cluster.ConnectTimeout = 10 * time.Second
	cluster.Timeout = 10 * time.Second

	session, err := o11ycassandra.NewSession(
		cluster,
		obs.TracerProvider(),
		obs.MeterProvider(),
		o11ycassandra.WithQueryText(exampleOptInEnabled("O11Y_CASSANDRA_QUERY_TEXT")),
	)
	if err != nil {
		return fmt.Errorf("create instrumented Cassandra session: %w", err)
	}
	defer session.Close()

	if err := ensureSchema(ctx, session); err != nil {
		return fmt.Errorf("ensure schema: %w", err)
	}

	logger := obs.Logger
	tracer := obs.Tracer("cassandra-example")

	logger.InfoContext(ctx, "Cassandra example started",
		slog.String("cassandra.addr", addr),
		slog.String("metrics_sink", "OTLP -> OTel Collector -> Prometheus"),
	)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		runCassandraCycle(ctx, logger, tracer, session)

		select {
		case <-ctx.Done():
			logger.InfoContext(ctx, "Cassandra example stopped")
			return nil
		case <-ticker.C:
		}
	}
}

func ensureSchema(ctx context.Context, session *gocql.Session) error {
	if err := session.Query(
		`CREATE KEYSPACE IF NOT EXISTS ` + keyspace +
			` WITH replication = {'class':'SimpleStrategy','replication_factor':1}`,
	).WithContext(ctx).Exec(); err != nil {
		return err
	}
	return session.Query(
		`CREATE TABLE IF NOT EXISTS ` + keyspace + `.events (id text PRIMARY KEY, body text)`,
	).WithContext(ctx).Exec()
}

func runCassandraCycle(
	ctx context.Context,
	logger *slog.Logger,
	tracer trace.Tracer,
	session *gocql.Session,
) {
	cycleCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cycleCtx, span := tracer.Start(cycleCtx, "cassandra-cycle")
	defer span.End()

	eventID := time.Now().UTC().Format("20060102T150405.000000000Z")
	fail := func(err error) {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		logger.ErrorContext(cycleCtx, "Cassandra cycle failed", slog.Any("error", err))
	}

	if err := session.Query(
		`INSERT INTO `+keyspace+`.events (id, body) VALUES (?, ?)`, eventID, "created",
	).WithContext(cycleCtx).Exec(); err != nil {
		fail(fmt.Errorf("insert event: %w", err))
		return
	}

	var body string
	if err := session.Query(
		`SELECT body FROM `+keyspace+`.events WHERE id = ?`, eventID,
	).WithContext(cycleCtx).Scan(&body); err != nil {
		fail(fmt.Errorf("select event: %w", err))
		return
	}

	// Batch writes go through ExecuteBatch so they produce one logical-batch
	// span (the driver's bare batch observer cannot identify the batch).
	batch := session.NewBatch(gocql.LoggedBatch).WithContext(cycleCtx)
	batch.Query(`INSERT INTO `+keyspace+`.events (id, body) VALUES (?, ?)`, eventID+":a", "batch-a")
	batch.Query(`INSERT INTO `+keyspace+`.events (id, body) VALUES (?, ?)`, eventID+":b", "batch-b")
	if err := o11ycassandra.ExecuteBatch(cycleCtx, session, batch); err != nil {
		fail(fmt.Errorf("execute batch: %w", err))
		return
	}

	logger.InfoContext(cycleCtx, "Cassandra cycle completed",
		slog.String("event.id", eventID),
		slog.String("event.body", body),
	)
}

func envOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func exampleOptInEnabled(key string) bool {
	v, ok := os.LookupEnv(key)
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
