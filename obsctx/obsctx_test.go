package obsctx_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/flywindy/o11y/o11ytest"
	"github.com/flywindy/o11y/obsctx"
)

type ctxKey string

func TestDetach_KeepsValuesButDropsCancelation(t *testing.T) {
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKey("k"), "v"))
	detached := obsctx.Detach(parent)

	cancel() // simulate the request ending

	if err := detached.Err(); err != nil {
		t.Fatalf("detached context must survive parent cancelation, got %v", err)
	}
	if got := detached.Value(ctxKey("k")); got != "v" {
		t.Fatalf("detached context lost its values: got %v, want v", got)
	}
	if _, ok := detached.Deadline(); ok {
		t.Fatal("Detach must not carry a deadline")
	}
}

func TestDetachWithTimeout_SurvivesParentButHonorsDeadline(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	detached, dcancel := obsctx.DetachWithTimeout(parent, 50*time.Millisecond)
	defer dcancel()

	cancel() // request ends immediately

	if err := detached.Err(); err != nil {
		t.Fatalf("detached context must survive parent cancelation, got %v", err)
	}
	if _, ok := detached.Deadline(); !ok {
		t.Fatal("DetachWithTimeout must carry a deadline")
	}

	select {
	case <-detached.Done():
		if !errors.Is(detached.Err(), context.DeadlineExceeded) {
			t.Fatalf("expected deadline exceeded, got %v", detached.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("timeout did not fire")
	}
}

// TestGo_SurvivesRequestEnd is the canonical regression test for the production
// incident: a request handler starts background work, the request ends (its
// context is canceled), and the background work must NOT see that cancelation.
func TestGo_SurvivesRequestEnd(t *testing.T) {
	reqCtx, endRequest := o11ytest.CanceledRequestContext()
	gotCtx := make(chan context.Context, 1)

	obsctx.Go(reqCtx, time.Second, func(ctx context.Context) {
		gotCtx <- ctx
	})

	select {
	case bg := <-gotCtx:
		// Capture the context the background work received, THEN end the
		// request, and only then assert. This is deterministic: it fails even
		// if the goroutine was scheduled first, because the assertion runs
		// after cancelation. (Asserting ctx.Err() inside the goroutine would be
		// timing-dependent — a raw request context read before endRequest still
		// reads nil and would wrongly pass.)
		endRequest()
		o11ytest.RequireNotCanceled(bg, t)
	case <-time.After(2 * time.Second):
		t.Fatal("background work did not run")
	}
}

func TestGo_RecoversPanic(_ *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	// A panic in fire-and-forget work must not crash the process.
	obsctx.Go(context.Background(), time.Second, func(context.Context) {
		defer wg.Done()
		panic("boom")
	})
	wg.Wait()
}
