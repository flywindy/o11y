package cassandra

import (
	"context"
	"errors"
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
	// CompareAndDelete (not Delete) guards against pointer-address reuse: if this
	// session is collected and a new one is allocated at the same address and
	// registered before this cleanup runs, deleting unconditionally would drop
	// the new session's entry. The cleanup arg must not reference the session
	// itself, or it would keep it alive and never run; the observer pointer is
	// safe and is exactly the value to compare against.
	runtime.AddCleanup(s, func(prev *observer) {
		sessionObservers.CompareAndDelete(key, prev)
	}, o)
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
//
// ctx is bound onto the batch via batch.WithContext(ctx) so it governs the
// driver call itself (cancellation/deadline), not just telemetry — keeping the
// public batch API context-first. A nil session or batch returns an error
// rather than panicking.
func ExecuteBatch(ctx context.Context, session *gocql.Session, batch *gocql.Batch) error {
	if session == nil {
		return errors.New("cassandra: session must not be nil")
	}
	if batch == nil {
		return errors.New("cassandra: batch must not be nil")
	}
	batch = batch.WithContext(ctx)

	obs, ok := lookupObserver(session)
	if !ok {
		return session.ExecuteBatch(batch)
	}

	start := time.Now()
	err := session.ExecuteBatch(batch)
	end := time.Now()

	namespace := batchNamespace(batch)
	obs.record(ctx, spanName("BATCH", ""), "BATCH", namespace, start, end, err, obs.batchAttrs(batch, namespace))
	return err
}

// batchNamespace resolves db.namespace for a batch. Each statement's effective
// keyspace is its explicit keyspace.table qualifier when present (authoritative,
// as on the query path), otherwise the driver-reported session keyspace. The
// batch's db.namespace is set only when every statement resolves to the same
// keyspace; a batch that genuinely spans multiple keyspaces returns "" so the
// attribute is omitted rather than mislabeling the batch with one of them
// (ADR 0019 §5).
func batchNamespace(batch *gocql.Batch) string {
	session := batch.Keyspace()
	resolved := ""
	for _, entry := range batch.Entries {
		_, ks, _ := parseStatement(entry.Stmt)
		if ks == "" {
			ks = session
		}
		if ks == "" {
			continue
		}
		if resolved == "" {
			resolved = ks
		} else if resolved != ks {
			return "" // batch spans multiple keyspaces; omit db.namespace
		}
	}
	if resolved != "" {
		return resolved
	}
	return session
}

// batchAttrs builds the span attributes for one logical batch, feeding
// db.operation.batch.size from the statement count (ADR 0019 §4).
func (o *observer) batchAttrs(batch *gocql.Batch, namespace string) []attribute.KeyValue {
	attrs := o.baseAttrs("BATCH", namespace, "", nil)
	if size := batch.Size(); size > 0 {
		attrs = append(attrs, semconv.DBOperationBatchSize(size))
	}
	return attrs
}
