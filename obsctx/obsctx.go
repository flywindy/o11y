// Package obsctx provides context helpers for carrying observability context
// (OpenTelemetry spans, baggage) into work that outlives the originating
// request, without inheriting the request's cancelation or deadline.
//
// Threading a request-scoped context (for example http.Request.Context or
// *gin.Context) into a background goroutine or a post-response write is a common
// way to "stitch traces together", but it is unsafe: the request context is
// canceled the moment the handler returns (or the client disconnects), so the
// background work is aborted mid-flight. This surfaces as "context canceled"
// and, for databases, dropped connections ("connection reset by peer"). The
// failure is timing dependent, so it often passes locally (low latency, one
// request at a time) and only appears under production latency and concurrency.
//
// The rule:
//
//   - Synchronous work that completes within the request: use the request
//     context directly (it should be canceled if the client goes away).
//   - Background / post-response / fire-and-forget work: use Detach (or Go) so
//     the work keeps the trace context but is not canceled when the request
//     ends, and give it its own timeout.
//
// Observability must be non-load-bearing: stitching a trace must never change a
// program's cancelation or lifetime semantics.
package obsctx

import (
	"context"
	"log/slog"
	"time"
)

// Detach returns a context that retains ctx's values (the OpenTelemetry span,
// baggage, and any other context values) but is never canceled by ctx and
// carries no deadline. Use it for work that must outlive the originating request
// so the work stays linked in the same trace without being canceled when the
// request ends.
//
// Detached work has no deadline; pair Detach with an explicit timeout
// (DetachWithTimeout, or context.WithTimeout) so it cannot hang forever.
func Detach(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}

// DetachWithTimeout is Detach plus an independent deadline so detached work
// cannot run forever. The caller MUST call the returned CancelFunc (typically
// via defer) to release resources.
func DetachWithTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(Detach(ctx), timeout)
}

// Go runs fn on a new goroutine with a context detached from reqCtx and bounded
// by timeout. It is the safe primitive for starting background work from a
// request handler: fn keeps reqCtx's trace context (so its spans stay in the
// same trace) but is not canceled when the request ends.
//
// Panics in fn are recovered and logged so a background failure cannot crash
// the process. The panic is logged through the process default slog logger
// (slog.Default), which is not necessarily the SDK-managed logger and does not
// guarantee trace correlation; applications that need correlated panic logs
// should configure slog.SetDefault at startup or log explicitly inside fn. Do
// not capture *gin.Context or the *http.Request inside fn; extract what you
// need before calling Go. fn receives the detached context and must use it for
// all downstream calls (DB, HTTP, ...).
func Go(reqCtx context.Context, timeout time.Duration, fn func(ctx context.Context)) {
	if fn == nil {
		return
	}
	ctx, cancel := DetachWithTimeout(reqCtx, timeout)
	go func() {
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				slog.ErrorContext(ctx, "obsctx: recovered panic in background work", slog.Any("panic", r))
			}
		}()
		fn(ctx)
	}()
}
