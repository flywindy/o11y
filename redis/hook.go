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
	"unicode/utf8"

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
		// Only record successful dials. semconv defines
		// db.client.connection.create_time as the time to create a connection;
		// folding failed-dial wall time into the same histogram skews the
		// percentiles operators use to size pools and tune backoff.
		if err == nil {
			h.connectionCreate.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(h.poolMetricAttrs...))
		}
		return conn, err
	}
}

func (h *redisHook) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		if h.disabled.Load() || h.skipUnparented(ctx) || h.isFilteredCommand(cmd) {
			return next(ctx, cmd)
		}

		operation := strings.ToUpper(cmd.FullName())
		if operation == "" {
			operation = strings.ToUpper(cmd.Name())
		}

		// User attributes go first so the SDK's own semconv attributes win on
		// key collisions: OTel resolves duplicate keys last-wins, and the
		// built-in keys (db.system.name, db.operation.name, server.*, etc.) are
		// part of this package's contract and must not be silently overridden by
		// WithAttributes.
		attrs := append([]attribute.KeyValue{}, h.cfg.attrs...)
		attrs = append(attrs, h.spanAttrs(operation)...)
		if h.cfg.commandTextEnabled {
			attrs = append(attrs, semconv.DBQueryText(truncateCommandText(commandText(cmd))))
		}

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
		if len(userCmds) == 0 || h.skipUnparented(ctx) || h.allFiltered(userCmds) {
			return next(ctx, cmds)
		}

		// User attributes go first so the SDK's built-in semconv keys win on
		// collision (OTel resolves duplicate keys last-wins). See ProcessHook.
		attrs := append([]attribute.KeyValue{}, h.cfg.attrs...)
		attrs = append(attrs, h.spanAttrs("pipeline")...)
		if len(userCmds) >= 2 {
			attrs = append(attrs, semconv.DBOperationBatchSize(len(userCmds)))
		}
		if h.cfg.commandTextEnabled {
			attrs = append(attrs, semconv.DBQueryText(truncateCommandText(pipelineText(userCmds))))
		}

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
	// Skip the default DB (0). go-redis always reports a DB, so emitting "0"
	// on every span adds a constant attribute that carries no operational
	// signal; an explicit non-default DB is meaningful and is recorded.
	if h.db == 0 {
		return ""
	}
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
	// Back the cut up to a rune boundary so the truncated text stays valid
	// UTF-8 even when the maxCommandTextLen-th byte falls inside a multi-byte
	// rune. Some OTLP exporters reject invalid UTF-8 outright.
	end := maxCommandTextLen
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end]
}

// isFilteredCommand reports whether a single command must be excluded from
// telemetry entirely (no span, no duration sample). Three categories qualify:
//
//   - Pub/Sub commands, which belong to the messaging.* model rather than db.*
//     (ADR 0013 §11) and are always excluded;
//   - connection-lifecycle commands the client issues itself on connect, which
//     are never an application unit of work and are always excluded;
//   - commands the caller opted out of via WithIgnoredCommands.
func (h *redisHook) isFilteredCommand(cmd goredis.Cmder) bool {
	name := strings.ToLower(cmd.Name())
	if isPubSubCommand(name) || isConnectionLifecycleCommand(name, cmd) {
		return true
	}
	if h.cfg.ignored != nil {
		if _, ok := h.cfg.ignored[name]; ok {
			return true
		}
	}
	return false
}

// allFiltered reports whether every command in a pipeline is individually
// filtered, in which case the whole pipeline span and sample are skipped.
func (h *redisHook) allFiltered(cmds []goredis.Cmder) bool {
	for _, cmd := range cmds {
		if !h.isFilteredCommand(cmd) {
			return false
		}
	}
	return true
}

// skipUnparented reports whether WithRequireParentSpan is set and the context
// carries no valid span context, meaning the operation is not part of a traced
// request and should be dropped.
func (h *redisHook) skipUnparented(ctx context.Context) bool {
	return h.cfg.requireParentSpan && !trace.SpanContextFromContext(ctx).IsValid()
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

// isConnectionLifecycleCommand reports whether a command is one the go-redis
// client issues itself when establishing or initialising a connection rather
// than one the application runs as a unit of work. Emitting db.* spans for these
// misrepresents application activity (and, for AUTH, would risk leaking
// credentials into db.query.text), so they are always filtered — like Pub/Sub.
//
// AUTH / HELLO / SELECT are matched by verb — go-redis v9 issues them itself on
// connect (HELLO for the RESP handshake, AUTH for credentials, SELECT for a
// non-zero DB). For CLIENT, only the auto-issued setup subcommands (SETINFO
// advertising the library name/version, SETNAME applying ClientName) are
// filtered; deliberate CLIENT subcommands such as LIST or KILL stay
// instrumented. Commands a client does not auto-issue (e.g. COMMAND) are left to
// WithIgnoredCommands so the always-filtered set stays strictly "never
// application work".
//
// name must be the lowercased command verb (cmd.Name()); it is passed in rather
// than recomputed so the per-command hot path lowercases once.
func isConnectionLifecycleCommand(name string, cmd goredis.Cmder) bool {
	switch name {
	case "auth", "hello", "select":
		return true
	case "client":
		return isClientSetupSubcommand(cmd)
	default:
		return false
	}
}

func isClientSetupSubcommand(cmd goredis.Cmder) bool {
	args := cmd.Args()
	if len(args) < 2 {
		return false
	}
	// go-redis accepts command arguments as either string or []byte (callers may
	// pass []byte to avoid allocations), so handle both rather than assuming
	// string and silently failing the type assertion.
	var sub string
	switch v := args[1].(type) {
	case string:
		sub = v
	case []byte:
		sub = string(v)
	default:
		return false
	}
	switch strings.ToLower(sub) {
	case "setinfo", "setname":
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

func pipelineError(execErr error, cmds []goredis.Cmder) error {
	for _, cmd := range cmds {
		if err := cmd.Err(); err != nil && !errors.Is(err, goredis.Nil) {
			return err
		}
	}
	return execErr
}
