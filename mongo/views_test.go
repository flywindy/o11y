package mongo

import (
	"testing"

	otelmongo "go.opentelemetry.io/contrib/instrumentation/go.mongodb.org/mongo-driver/v2/mongo/otelmongo"

	"github.com/flywindy/o11y/internal/views"
)

// TestMongoContribScopeMatchesUpstream guards the one constant the SDK has to
// copy rather than reference.
//
// internal/views must not import otelmongo — doing so would link the mongo
// driver into the root o11y package, which is the cost ADR 0026 Option A exists
// to remove — so views.MongoContribScope holds a literal copy of
// otelmongo.ScopeName. This package already imports otelmongo, so it is the one
// place that can compare them.
//
// Without this test an otelmongo release that renames its scope would detach
// the db.client.operation.duration view from the instrument it targets, and the
// symptom would be silent: the metric keeps being emitted, but with the
// contrib instrument's baked-in boundaries and an unbounded label set, which
// looks like a working histogram until someone queries it.
func TestMongoContribScopeMatchesUpstream(t *testing.T) {
	if views.MongoContribScope != otelmongo.ScopeName {
		t.Fatalf("views.MongoContribScope is stale: got %q, otelmongo.ScopeName is %q\n"+
			"otelmongo renamed its instrumentation scope; update the constant in "+
			"internal/views/mongo.go so the operation-duration view keeps matching",
			views.MongoContribScope, otelmongo.ScopeName)
	}
}

// TestMongoScopeMatchesInstrumentationName pins the other direction: the pool
// views are scoped to views.MongoScope, and this package records its pool
// metrics under instrumentationName. They are the same constant by aliasing
// rather than by copy, so this asserts the alias is still in place — a future
// edit that re-inlines the string here would silently detach the pool views.
func TestMongoScopeMatchesInstrumentationName(t *testing.T) {
	if instrumentationName != views.MongoScope {
		t.Fatalf("instrumentationName %q no longer matches views.MongoScope %q: the pool views would not match the instruments this package emits",
			instrumentationName, views.MongoScope)
	}
}
