package cassandra

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"strings"
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
	return instrumentBatch(ctx, session, batch, func(b *gocql.Batch) error {
		return session.ExecuteBatch(b)
	})
}

// ExecuteBatchCAS is the instrumented counterpart of (*gocql.Session).ExecuteBatchCAS
// for lightweight-transaction (conditional) batches. It produces the same single
// BATCH span and metric as ExecuteBatch and returns the driver's applied flag and
// result iterator unchanged. Without this seam a CAS batch would have no
// instrumented entry point and emit no telemetry (the BatchObserver is not wired;
// ADR 0019 §4).
func ExecuteBatchCAS(
	ctx context.Context,
	session *gocql.Session,
	batch *gocql.Batch,
	dest ...interface{},
) (applied bool, iter *gocql.Iter, err error) {
	err = instrumentBatch(ctx, session, batch, func(b *gocql.Batch) error {
		applied, iter, err = session.ExecuteBatchCAS(b, dest...)
		return err
	})
	return applied, iter, err
}

// MapExecuteBatchCAS is the instrumented counterpart of
// (*gocql.Session).MapExecuteBatchCAS, mirroring ExecuteBatchCAS for the
// map-destination form.
func MapExecuteBatchCAS(
	ctx context.Context,
	session *gocql.Session,
	batch *gocql.Batch,
	dest map[string]interface{},
) (applied bool, iter *gocql.Iter, err error) {
	err = instrumentBatch(ctx, session, batch, func(b *gocql.Batch) error {
		applied, iter, err = session.MapExecuteBatchCAS(b, dest)
		return err
	})
	return applied, iter, err
}

// instrumentBatch is the shared batch seam: it validates arguments, binds ctx
// onto the batch, runs the driver call via exec, and records one BATCH span plus
// the operation-duration metric. exec receives the context-bound batch so each
// public entry point only supplies the specific driver call (plain vs CAS). A
// session not created by NewSession runs exec without instrumentation.
func instrumentBatch(
	ctx context.Context,
	session *gocql.Session,
	batch *gocql.Batch,
	exec func(*gocql.Batch) error,
) error {
	if session == nil {
		return errors.New("cassandra: session must not be nil")
	}
	if batch == nil {
		return errors.New("cassandra: batch must not be nil")
	}
	batch = batch.WithContext(ctx)

	obs, ok := lookupObserver(session)
	if !ok {
		return exec(batch)
	}

	start := time.Now()
	err := exec(batch)
	end := time.Now()

	tgt := batchTarget(batchTargets(batch))
	// The batch seam has no per-statement host (gocql's ExecuteBatch* take no
	// observer host here), so server.* is the configured contact point.
	obs.record(ctx, spanName(tgt.operation, tgt.table), tgt, obs.server, start, end, err, obs.batchAttrs(batch, tgt))
	return err
}

// batchTarget builds the target for one logical batch from the namespace/table
// batchTargets resolved. The operation is always "BATCH": gocql executes the
// statements as a single server-side operation, so the per-statement verbs are
// not what the span and metric describe.
func batchTarget(namespace, table string) target {
	return target{operation: "BATCH", keyspace: namespace, table: table}
}

// batchTargets resolves db.namespace and db.collection.name for a batch in a
// single pass over its statements.
//
// Namespace: each statement's effective keyspace is its explicit keyspace.table
// qualifier when present (authoritative, as on the query path), otherwise the
// driver-reported session keyspace; the batch reports a namespace only when every
// statement resolves to the same non-empty one. A multi-keyspace batch, or one
// with any unresolved statement (unqualified with no session keyspace), omits it
// rather than mislabeling. Table: reported only when every statement addresses
// the same fully-qualified table (same effective keyspace and bare name), so a
// common single-table batch can be grouped by table while the same bare name in
// two keyspaces is not collapsed; a mixed or unparseable batch omits it.
// (ADR 0019 §5.)
func batchTargets(batch *gocql.Batch) (namespace, table string) {
	session := batch.Keyspace()
	resolvedNS, nsConflict := "", false
	resolvedTbl, resolvedKey, tblConflict := "", "", false
	for _, entry := range batch.Entries {
		_, ks, tbl := parseStatement(entry.Stmt)
		if ks == "" {
			ks = session
		}
		if !nsConflict {
			switch {
			case ks == "":
				// An unqualified statement with no session keyspace has an
				// unknown effective keyspace (and would typically fail to
				// execute); treat it as ambiguity and omit db.namespace rather
				// than letting a sibling qualified statement label the batch.
				resolvedNS, nsConflict = "", true
			case resolvedNS == "":
				resolvedNS = ks
			case resolvedNS != ks:
				resolvedNS, nsConflict = "", true
			}
		}
		if !tblConflict {
			// Compare the fully-qualified <effective-keyspace>.<table> so the same
			// bare table name in two keyspaces (ks_a.events vs ks_b.events) is not
			// collapsed into one db.collection.name.
			key := ks + "." + tbl
			switch {
			case tbl == "":
				// An unparseable/empty table means we cannot prove the batch is
				// single-table, so omit db.collection.name rather than guess.
				resolvedTbl, tblConflict = "", true
			case resolvedKey == "":
				resolvedTbl, resolvedKey = tbl, key
			case resolvedKey != key:
				resolvedTbl, tblConflict = "", true
			}
		}
	}
	if resolvedNS == "" && !nsConflict {
		resolvedNS = session
	}
	return resolvedNS, resolvedTbl
}

// batchAttrs builds the span attributes for one logical batch, feeding
// db.operation.batch.size from the statement count (ADR 0019 §4). db.query.text
// is appended only under WithQueryText, mirroring the query path; the batch's
// statements are joined since a batch has no single statement.
func (o *observer) batchAttrs(batch *gocql.Batch, tgt target) []attribute.KeyValue {
	attrs := o.baseAttrs(tgt, o.server, nil)
	if size := batch.Size(); size > 0 {
		attrs = append(attrs, semconv.DBOperationBatchSize(size))
	}
	if o.cfg.queryTextEnabled {
		if text := batchQueryText(batch); text != "" {
			attrs = append(attrs, semconv.DBQueryText(text))
		}
	}
	return attrs
}

// batchQueryText joins the batch's statement texts for db.query.text under the
// WithQueryText opt-in. Bound values are never included (gocql keeps them in
// entry.Args, which this does not read).
func batchQueryText(batch *gocql.Batch) string {
	stmts := make([]string, 0, len(batch.Entries))
	for _, entry := range batch.Entries {
		if entry.Stmt != "" {
			stmts = append(stmts, entry.Stmt)
		}
	}
	return strings.Join(stmts, "; ")
}
