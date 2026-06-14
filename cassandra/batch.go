package cassandra

import (
	"context"
	"reflect"
	"runtime"
	"sync"
	"time"

	"github.com/gocql/gocql"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

// sessionObservers maps an instrumented *gocql.Session to its observer so the
// SDK-owned batch seam (ExecuteBatch) can find the tracer and instruments that
// NewSession created. It is keyed by the session pointer value; entries are
// removed when the session is garbage-collected.
//
// Looking the observer up here (rather than threading it through a wrapper type)
// keeps NewSession's return type the plain *gocql.Session callers already use.
var sessionObservers sync.Map // uintptr -> *observer

func registerSession(s *gocql.Session, o *observer) {
	key := sessionKey(s)
	sessionObservers.Store(key, o)
	// The cleanup arg must not reference the session, or it would keep it alive
	// and the cleanup would never run; the bare key value is safe.
	runtime.AddCleanup(s, func(k uintptr) { sessionObservers.Delete(k) }, key)
}

func sessionKey(s *gocql.Session) uintptr {
	return reflect.ValueOf(s).Pointer()
}

func lookupObserver(s *gocql.Session) (*observer, bool) {
	v, ok := sessionObservers.Load(sessionKey(s))
	if !ok {
		return nil, false
	}
	return v.(*observer), true
}

// ExecuteBatch runs batch on session and records exactly one CLIENT span per
// logical batch, plus the db.client.operation.duration metric.
//
// This is the SDK-owned batch seam (ADR 0019 §4). gocql's BatchObserver fires
// once for the batch and once per statement with no batch identity, so it
// cannot reliably coalesce into one span; ExecuteBatch owns the span instead and
// feeds db.operation.batch.size from the statement count.
//
// If session was not created by NewSession (no registered observer), ExecuteBatch
// executes the batch without instrumentation, so it is always safe to call.
func ExecuteBatch(ctx context.Context, session *gocql.Session, batch *gocql.Batch) error {
	obs, ok := lookupObserver(session)
	if !ok {
		return session.ExecuteBatch(batch)
	}

	start := time.Now()
	err := session.ExecuteBatch(batch)
	end := time.Now()

	obs.record(ctx, spanName("BATCH", ""), "BATCH", batch.Keyspace(), start, end, err, obs.batchAttrs(batch))
	return err
}

// batchAttrs builds the span attributes for one logical batch, feeding
// db.operation.batch.size from the statement count (ADR 0019 §4).
func (o *observer) batchAttrs(batch *gocql.Batch) []attribute.KeyValue {
	attrs := o.baseAttrs("BATCH", batch.Keyspace(), "", nil)
	if size := batch.Size(); size > 0 {
		attrs = append(attrs, semconv.DBOperationBatchSize(size))
	}
	return attrs
}
