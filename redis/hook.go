package redis

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	instrumentationName = "github.com/flywindy/o11y/redis"
	maxCommandTextLen   = 1024

	redisErrorKindKey = attribute.Key("redis.error.kind")
)

type redisHook struct {
	tracer            trace.Tracer
	operationDuration metric.Float64Histogram
	connectionCreate  metric.Float64Histogram
	cfg               config
	address           addressAttrs
	db                int
	poolMetricAttrs   []attribute.KeyValue
	disabled          *atomic.Bool
}

type addressAttrs struct {
	addr string
	host string
	port int
}

func newRedisHook(
	tp trace.TracerProvider,
	operationDuration metric.Float64Histogram,
	connectionCreate metric.Float64Histogram,
	cfg config,
	client *goredis.Client,
	poolName string,
	disabled *atomic.Bool,
) *redisHook {
	addr := parseAddress(client.Options().Addr)
	poolMetricAttrs := []attribute.KeyValue{
		semconv.DBSystemNameRedis,
		attribute.String("db.client.connection.pool.name", poolName),
		semconv.ServerAddress(addr.host),
	}
	if addr.port > 0 {
		poolMetricAttrs = append(poolMetricAttrs, semconv.ServerPort(addr.port))
	}

	return &redisHook{
		tracer: tp.Tracer(
			instrumentationName,
			trace.WithSchemaURL(semconv.SchemaURL),
		),
		operationDuration: operationDuration,
		connectionCreate:  connectionCreate,
		cfg:               cfg,
		address:           addr,
		db:                client.Options().DB,
		poolMetricAttrs:   poolMetricAttrs,
		disabled:          disabled,
	}
}

func (h *redisHook) DialHook(next goredis.DialHook) goredis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if h.disabled.Load() {
			return next(ctx, network, addr)
		}

		start := time.Now()
		conn, err := next(ctx, network, addr)
		h.connectionCreate.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(h.poolMetricAttrs...))
		return conn, err
	}
}

func (h *redisHook) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		if h.disabled.Load() || isPubSubCommand(cmd.Name()) {
			return next(ctx, cmd)
		}

		operation := strings.ToUpper(cmd.FullName())
		if operation == "" {
			operation = strings.ToUpper(cmd.Name())
		}

		attrs := h.spanAttrs(operation)
		if h.cfg.commandTextEnabled {
			attrs = append(attrs, semconv.DBQueryText(truncateCommandText(commandText(cmd))))
		}
		attrs = append(attrs, h.cfg.attrs...)

		ctx, span := h.tracer.Start(ctx, "redis."+operation,
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(attrs...),
		)
		start := time.Now()
		err := next(ctx, cmd)
		h.finish(ctx, span, time.Since(start), operation, err)
		return err
	}
}

func (h *redisHook) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		if h.disabled.Load() {
			return next(ctx, cmds)
		}

		userCmds := userPipelineCommands(cmds)
		if len(userCmds) == 0 || allPubSubCommands(userCmds) {
			return next(ctx, cmds)
		}

		attrs := h.spanAttrs("pipeline")
		if len(userCmds) >= 2 {
			attrs = append(attrs, semconv.DBOperationBatchSize(len(userCmds)))
		}
		if h.cfg.commandTextEnabled {
			attrs = append(attrs, semconv.DBQueryText(truncateCommandText(pipelineText(userCmds))))
		}
		attrs = append(attrs, h.cfg.attrs...)

		ctx, span := h.tracer.Start(ctx, "redis.pipeline",
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(attrs...),
		)
		start := time.Now()
		err := next(ctx, cmds)
		observedErr := pipelineError(err, userCmds)
		h.finish(ctx, span, time.Since(start), "pipeline", observedErr)
		return err
	}
}

func (h *redisHook) spanAttrs(operation string) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		semconv.DBSystemNameRedis,
		semconv.DBOperationName(operation),
		semconv.ServerAddress(h.address.host),
	}
	if h.address.port > 0 {
		attrs = append(attrs, semconv.ServerPort(h.address.port))
	}
	if ns := h.namespace(); ns != "" {
		attrs = append(attrs, semconv.DBNamespace(ns))
	}
	return attrs
}

func (h *redisHook) namespace() string {
	return strconv.Itoa(h.db)
}

func (h *redisHook) finish(
	ctx context.Context,
	span trace.Span,
	duration time.Duration,
	operation string,
	err error,
) {
	defer span.End()

	metricAttrs := []attribute.KeyValue{
		semconv.DBSystemNameRedis,
		semconv.DBOperationName(operation),
		semconv.ServerAddress(h.address.host),
	}
	if h.address.port > 0 {
		metricAttrs = append(metricAttrs, semconv.ServerPort(h.address.port))
	}

	if err != nil && !errors.Is(err, goredis.Nil) {
		errType := errorType(err)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.SetAttributes(semconv.ErrorTypeKey.String(errType))
		metricAttrs = append(metricAttrs, semconv.ErrorTypeKey.String(errType))

		if kind, ok := redisErrorKind(err); ok {
			span.SetAttributes(redisErrorKindKey.String(kind))
		}
	}

	h.operationDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(metricAttrs...))
}

func parseAddress(addr string) addressAttrs {
	out := addressAttrs{addr: addr, host: addr}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return out
	}
	out.host = host
	if parsed, err := strconv.Atoi(port); err == nil {
		out.port = parsed
	}
	return out
}

func errorType(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "context.DeadlineExceeded"
	case errors.Is(err, context.Canceled):
		return "context.Canceled"
	default:
		if typ := reflect.TypeOf(err); typ != nil {
			return typ.String()
		}
		return "_OTHER"
	}
}

func redisErrorKind(err error) (string, bool) {
	switch {
	case errors.Is(err, goredis.ErrPoolTimeout):
		return "pool_timeout", true
	case errors.Is(err, context.DeadlineExceeded):
		return "client_timeout", true
	case errors.Is(err, context.Canceled):
		return "client_canceled", true
	default:
		return "", false
	}
}

func commandText(cmd goredis.Cmder) string {
	args := cmd.Args()
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		parts = append(parts, fmt.Sprint(arg))
	}
	return strings.Join(parts, " ")
}

func pipelineText(cmds []goredis.Cmder) string {
	parts := make([]string, 0, len(cmds))
	for _, cmd := range cmds {
		parts = append(parts, commandText(cmd))
	}
	return strings.Join(parts, "; ")
}

func truncateCommandText(text string) string {
	if len(text) <= maxCommandTextLen {
		return text
	}
	return text[:maxCommandTextLen]
}

func isPubSubCommand(name string) bool {
	switch strings.ToLower(name) {
	case "publish", "spublish", "subscribe", "psubscribe", "ssubscribe",
		"unsubscribe", "punsubscribe", "sunsubscribe", "pubsub":
		return true
	default:
		return false
	}
}

func userPipelineCommands(cmds []goredis.Cmder) []goredis.Cmder {
	out := make([]goredis.Cmder, 0, len(cmds))
	for _, cmd := range cmds {
		switch strings.ToLower(cmd.Name()) {
		case "multi", "exec":
			continue
		default:
			out = append(out, cmd)
		}
	}
	return out
}

func allPubSubCommands(cmds []goredis.Cmder) bool {
	for _, cmd := range cmds {
		if !isPubSubCommand(cmd.Name()) {
			return false
		}
	}
	return true
}

func pipelineError(execErr error, cmds []goredis.Cmder) error {
	for _, cmd := range cmds {
		if err := cmd.Err(); err != nil && !errors.Is(err, goredis.Nil) {
			return err
		}
	}
	return execErr
}
