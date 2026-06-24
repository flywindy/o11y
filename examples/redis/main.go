// Package main demonstrates Redis tracing and metrics through the o11y Redis wrapper.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/flywindy/o11y"
	o11yredis "github.com/flywindy/o11y/redis"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	defaultRedisAddr = "localhost:6379"
	keyPrefix        = "o11y:examples:redis"
)

func main() {
	if err := run(); err != nil {
		slog.ErrorContext(context.Background(), "Redis example failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	obs, err := o11y.Init(ctx,
		o11y.WithServiceName("redis-example"),
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

	redisAddr := envOrDefault("REDIS_ADDR", defaultRedisAddr)
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer rdb.Close()

	opts := []o11yredis.Option{
		o11yredis.WithPoolName("redis-example"),
		o11yredis.WithCommandTextEnabled(exampleOptInEnabled("O11Y_REDIS_COMMAND_TEXT")),
	}
	// Noise-trimming options (see runLivenessProbe for how they differ):
	//   O11Y_REDIS_IGNORE_COMMANDS=ping,info  -> drop those verbs everywhere.
	//   O11Y_REDIS_REQUIRE_PARENT_SPAN=true   -> drop commands with no parent span.
	if ignored := envList("O11Y_REDIS_IGNORE_COMMANDS"); len(ignored) > 0 {
		opts = append(opts, o11yredis.WithIgnoredCommands(ignored...))
	}
	if exampleOptInEnabled("O11Y_REDIS_REQUIRE_PARENT_SPAN") {
		opts = append(opts, o11yredis.WithRequireParentSpan(true))
	}

	_, err = o11yredis.Wrap(rdb, obs.TracerProvider(), obs.MeterProvider(), opts...)
	if err != nil {
		return fmt.Errorf("instrument Redis client: %w", err)
	}

	logger := obs.Logger
	tracer := obs.Tracer("redis-example")

	logger.InfoContext(ctx, "Redis example started",
		slog.String("redis.addr", redisAddr),
		slog.String("metrics_sink", "OTLP -> OTel Collector -> Prometheus"),
	)

	// Background health check: a PING with no parent span, modeling the noisy
	// liveness probes that motivate WithRequireParentSpan / WithIgnoredCommands.
	go runLivenessProbe(ctx, logger, rdb)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		runRedisCycle(ctx, logger, tracer, rdb)

		select {
		case <-ctx.Done():
			logger.InfoContext(ctx, "Redis example stopped")
			return nil
		case <-ticker.C:
		}
	}
}

func runRedisCycle(
	ctx context.Context,
	logger *slog.Logger,
	tracer trace.Tracer,
	rdb *redis.Client,
) {
	cycleCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cycleCtx, span := tracer.Start(cycleCtx, "redis-cycle")
	defer span.End()

	eventID := time.Now().UTC().Format("20060102T150405.000000000Z")
	key := keyPrefix + ":" + eventID
	fail := func(err error) {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		logger.ErrorContext(cycleCtx, "Redis cycle failed", slog.Any("error", err))
	}

	if err := rdb.Ping(cycleCtx).Err(); err != nil {
		fail(fmt.Errorf("ping Redis: %w", err))
		return
	}
	if err := rdb.Set(cycleCtx, key, "created", 30*time.Second).Err(); err != nil {
		fail(fmt.Errorf("set %s: %w", key, err))
		return
	}
	value, err := rdb.Get(cycleCtx, key).Result()
	if err != nil {
		fail(fmt.Errorf("get %s: %w", key, err))
		return
	}
	if _, err := rdb.Get(cycleCtx, key+":missing").Result(); err != nil && !errors.Is(err, redis.Nil) {
		fail(fmt.Errorf("get missing key: %w", err))
		return
	}

	pipe := rdb.Pipeline()
	pipe.Incr(cycleCtx, keyPrefix+":cycles")
	pipe.Expire(cycleCtx, keyPrefix+":cycles", time.Minute)
	pipe.Del(cycleCtx, key)
	if _, err := pipe.Exec(cycleCtx); err != nil {
		fail(fmt.Errorf("execute pipeline: %w", err))
		return
	}

	logger.InfoContext(cycleCtx, "Redis cycle completed",
		slog.String("key", key),
		slog.String("value", value),
	)
}

// runLivenessProbe issues a PING on a bare context (no parent span) on an
// interval, modeling a background health check. It exists to show how the two
// noise-trimming options differ:
//
//   - WithRequireParentSpan(true) drops this PING (no parent span) but keeps the
//     request-bound PING inside runRedisCycle, which runs under the "redis-cycle"
//     span.
//   - WithIgnoredCommands("ping") drops both PINGs, because it matches the
//     command verb regardless of context.
//
// So if an app "sees PING" and only wants it gone, WithIgnoredCommands("ping")
// is the precise fix and is sufficient on its own — WithRequireParentSpan only
// helps when the PING is genuinely unparented, and would miss a PING issued
// inside a traced request (e.g. a /healthz handler behind instrumented HTTP).
func runLivenessProbe(ctx context.Context, logger *slog.Logger, rdb *redis.Client) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := rdb.Ping(ctx).Err(); err != nil {
				logger.WarnContext(ctx, "Redis liveness probe failed", slog.Any("error", err))
			}
		}
	}
}

func envOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envList(key string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
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
