// Package main demonstrates MinIO/S3 object-storage tracing and metrics
// through the o11y MinIO wrapper.
//
// The example points at a local MinIO (k8s/infrastructure/base/minio.yaml
// for the cluster spec, or `docker run -p 9000:9000 -p 9001:9001 \
// quay.io/minio/minio server /data --console-address :9001`). It loops
// through the wrapped data-plane methods so every operation appears as a
// SpanKindClient span: PutObject, StatObject, GetObject, RemoveObject.
// With WithHTTPChildSpans enabled, the underlying HTTP round-trips appear
// as child spans of each logical operation.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/flywindy/o11y"
	o11yminio "github.com/flywindy/o11y/minio"
	miniogo "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	bucket      = "media"
	objectKey   = "videos/2026/clip.bin"
	objectSize  = 64 * 1024
	defaultAddr = "localhost:9000"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		slog.ErrorContext(context.Background(), "MinIO example failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	obs, err := o11y.Init(ctx,
		o11y.WithServiceName("minio-example"),
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

	endpoint := envOr("MINIO_ENDPOINT", defaultAddr)
	accessKey := envOr("MINIO_ACCESS_KEY", "minioadmin")
	secretKey := envOr("MINIO_SECRET_KEY", "minioadmin")

	client, err := o11yminio.New(
		endpoint,
		&miniogo.Options{
			Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
			Secure: false,
		},
		obs.TracerProvider(),
		obs.MeterProvider(),
		obs.Propagator,
		o11yminio.WithHTTPChildSpans(true),
	)
	if err != nil {
		return fmt.Errorf("construct MinIO client: %w", err)
	}

	if err := ensureBucket(ctx, client); err != nil {
		return fmt.Errorf("ensure bucket: %w", err)
	}

	tracer := obs.Tracer("minio-example")
	logger := obs.Logger
	logger.InfoContext(ctx, "MinIO example started",
		slog.String("endpoint", endpoint),
		slog.String("bucket", bucket),
	)

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		runCycle(ctx, logger, tracer, client)
		select {
		case <-ctx.Done():
			logger.InfoContext(ctx, "MinIO example stopped")
			return nil
		case <-ticker.C:
		}
	}
}

func ensureBucket(ctx context.Context, client *o11yminio.Client) error {
	exists, err := client.BucketExists(ctx, bucket)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return client.MakeBucket(ctx, bucket, miniogo.MakeBucketOptions{})
}

func runCycle(
	ctx context.Context,
	logger *slog.Logger,
	tracer trace.Tracer,
	client *o11yminio.Client,
) {
	cycleCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cycleCtx, span := tracer.Start(cycleCtx, "minio-cycle")
	defer span.End()

	payload := make([]byte, objectSize)
	for i := range payload {
		payload[i] = byte(i % 251)
	}

	// PutObject — span captures the full upload (incl. multipart when
	// minio-go chooses that path).
	if _, err := client.PutObject(cycleCtx, bucket, objectKey, bytes.NewReader(payload),
		int64(len(payload)), miniogo.PutObjectOptions{ContentType: "application/octet-stream"}); err != nil {
		failCycle(cycleCtx, logger, span, "PutObject", err)
		return
	}

	// StatObject — HEAD round-trip.
	info, err := client.StatObject(cycleCtx, bucket, objectKey, miniogo.StatObjectOptions{})
	if err != nil {
		failCycle(cycleCtx, logger, span, "StatObject", err)
		return
	}
	logger.InfoContext(cycleCtx, "object stat",
		slog.String("key", info.Key),
		slog.Int64("size", info.Size),
	)

	// GetObject — thin logical span; the Read below issues the real GET,
	// which surfaces as an HTTP child of the caller span that was active
	// at GetObject time (ADR 0018 §5).
	obj, err := client.GetObject(cycleCtx, bucket, objectKey, miniogo.GetObjectOptions{})
	if err != nil {
		failCycle(cycleCtx, logger, span, "GetObject", err)
		return
	}
	read, err := io.ReadAll(obj)
	_ = obj.Close()
	if err != nil {
		failCycle(cycleCtx, logger, span, "GetObject read", err)
		return
	}
	if len(read) != len(payload) {
		failCycle(cycleCtx, logger, span, "GetObject content",
			fmt.Errorf("size mismatch: read %d, want %d", len(read), len(payload)))
		return
	}

	// RemoveObject — closes out the cycle.
	if err := client.RemoveObject(cycleCtx, bucket, objectKey, miniogo.RemoveObjectOptions{}); err != nil {
		failCycle(cycleCtx, logger, span, "RemoveObject", err)
		return
	}
	logger.InfoContext(cycleCtx, "cycle complete",
		slog.Int("bytes", len(read)),
	)
}

func failCycle(ctx context.Context, logger *slog.Logger, span trace.Span, step string, err error) {
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	logger.ErrorContext(ctx, step+" failed", slog.Any("error", err))
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
