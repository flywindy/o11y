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
	started := make(chan struct{})
	released := make(chan struct{})
	errc := make(chan error, 1)

	obsctx.Go(reqCtx, time.Second, func(ctx context.Context) {
		close(started)
		<-released        // keep fn running across endRequest()
		errc <- ctx.Err() // read the background context's state, after the request ended but before fn returns
	})

	<-started
	endRequest() // the request ends while the background work is still running
	close(released)

	// Deterministic: errc carries ctx.Err() read strictly after endRequest and
	// before fn returns (so before Go's own defer cancel()). A correct detach
	// reads nil; a raw request context would read context.Canceled and fail —
	// regardless of goroutine scheduling.
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("background work inherited the request cancelation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("background work did not finish")
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
