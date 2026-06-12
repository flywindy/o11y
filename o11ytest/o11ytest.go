// Package o11ytest provides test helpers for asserting that background work is
// correctly detached from a request's context lifecycle (see package
// github.com/flywindy/o11y/obsctx).
//
// The helpers turn a timing-dependent production heisenbug — background work
// inheriting the request's cancelation — into a deterministic, CI-friendly
// assertion that does not depend on traffic, latency, or a real database.
package o11ytest

import (
	"context"
	"testing"
)

// CanceledRequestContext returns a context that mimics an in-flight HTTP request
// and an endRequest func that cancels it — exactly as net/http cancels
// Request.Context when the handler returns (or the client disconnects). Use it
// to prove background work survives request completion: kick off the work with
// the returned ctx, call endRequest, then assert the work's context is still
// live (see RequireNotCanceled).
func CanceledRequestContext() (ctx context.Context, endRequest func()) {
	return context.WithCancel(context.Background())
}

// RequireNotCanceled fails the test if ctx is already canceled. Call it from the
// background operation (after the request has ended) to assert the work was
// detached from the request context rather than inheriting its cancelation.
func RequireNotCanceled(ctx context.Context, t testing.TB) {
	t.Helper()
	if err := ctx.Err(); err != nil {
		t.Fatalf("context is canceled (%v): background work inherited the request's "+
			"cancelation. Use obsctx.Detach / DetachWithTimeout / Go for work that "+
			"outlives the request.", err)
	}
}
