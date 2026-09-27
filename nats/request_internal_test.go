package nats

import (
	"context"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
)

// deadlinePassedCtx is past its deadline but has not been cancelled yet: the
// state a real context is in for the instant between its deadline and its
// timer running, when ctx.Err() is still nil.
type deadlinePassedCtx struct{ context.Context }

func (deadlinePassedCtx) Deadline() (time.Time, bool) { return time.Now().Add(-time.Millisecond), true }

// TestWithRequestTimeout_SpentBudgetIsNotATimeout pins that a ctx already past
// its deadline at the call is classified from the deadline, not from
// ctx.Err(), so the answer does not depend on whether its timer has run; that
// the request is still made, so the traced path records its span; and that it
// is made under a context that is already done, so nats.go rejects it before
// sending even though ctx itself is not cancelled yet.
func TestWithRequestTimeout_SpentBudgetIsNotATimeout(t *testing.T) {
	called := false
	_, err := withRequestTimeout(deadlinePassedCtx{context.Background()}, time.Second,
		func(reqCtx context.Context) (*natsgo.Msg, error) {
			called = true
			assert.ErrorIs(t, reqCtx.Err(), context.DeadlineExceeded, "the request context must already be done")
			return nil, reqCtx.Err()
		})

	assert.True(t, called, "the request still goes through upstream, which records the span")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NotErrorIs(t, err, natsgo.ErrTimeout)
}

// TestWithRequestTimeout_NoTimeLeftIsNotATimeout covers timeouts that leave no
// time to send: zero, negative, and positive but already over by the time the
// request would start. Each must run under a context that is already done and
// come back unwrapped, so a retry-on-timeout loop computing its timeout from a
// remaining budget stops instead of spinning.
func TestWithRequestTimeout_NoTimeLeftIsNotATimeout(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Second, time.Nanosecond} {
		t.Run(timeout.String(), func(t *testing.T) {
			_, err := withRequestTimeout(context.Background(), timeout,
				func(reqCtx context.Context) (*natsgo.Msg, error) {
					assert.ErrorIs(t, reqCtx.Err(), context.DeadlineExceeded, "the request context must already be done")
					deadline, ok := reqCtx.Deadline()
					assert.True(t, ok)
					assert.WithinDuration(t, time.Now(), deadline, 5*time.Second, "a real deadline, not the zero time")
					return nil, reqCtx.Err()
				})

			assert.ErrorIs(t, err, context.DeadlineExceeded)
			assert.NotErrorIs(t, err, natsgo.ErrTimeout)
		})
	}
}

// TestWithRequestTimeout_NilCtx pins that a nil ctx returns
// nats.ErrInvalidContext, as nats.go does, rather than panicking.
func TestWithRequestTimeout_NilCtx(t *testing.T) {
	called := false
	var nilCtx context.Context
	_, err := withRequestTimeout(nilCtx, time.Second, func(context.Context) (*natsgo.Msg, error) {
		called = true
		return nil, nil
	})
	assert.ErrorIs(t, err, natsgo.ErrInvalidContext)
	assert.False(t, called)
}
