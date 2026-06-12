package o11ytest_test

import (
	"testing"

	"github.com/flywindy/o11y/o11ytest"
	"github.com/flywindy/o11y/obsctx"
)

func TestCanceledRequestContext(t *testing.T) {
	ctx, endRequest := o11ytest.CanceledRequestContext()
	if err := ctx.Err(); err != nil {
		t.Fatalf("context must be live before endRequest, got %v", err)
	}
	endRequest()
	if ctx.Err() == nil {
		t.Fatal("context must be canceled after endRequest")
	}
}

func TestRequireNotCanceled_PassesForDetachedContext(t *testing.T) {
	ctx, endRequest := o11ytest.CanceledRequestContext()
	detached := obsctx.Detach(ctx)
	endRequest() // request ends; detached work must remain live

	// This is the assertion application background work should make. It passes
	// because the work was detached; it would fail on a raw request context.
	o11ytest.RequireNotCanceled(t, detached)
}
