//go:build ignore

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSemconvKeys(t *testing.T) {
	keys, err := semconvKeys()
	if err != nil {
		t.Fatal(err)
	}
	// A partial parse, not only an empty one, must fail here: the pinned
	// package declares several hundred keys across its files.
	if len(keys) < 500 {
		t.Errorf("semconvKeys found %d keys, want several hundred", len(keys))
	}
	for _, key := range []string{"service.name", "server.address", "db.system.name", "error.type", "messaging.destination.name"} {
		if !keys[key] {
			t.Errorf("semconvKeys is missing %q", key)
		}
	}
	// The keys Core Principle 5 warns about are an earlier version's, which is
	// the whole reason Rule 2 exists: Rule 1 cannot match what is not here.
	for _, key := range []string{"db.statement", "db.system", "db.cassandra.table", "db.cassandra.consistency_level"} {
		if keys[key] {
			t.Errorf("semconvKeys holds %q, which the pinned package does not declare", key)
		}
	}
}

// TestAttributeKeyFuncs pins that the key positions are read correctly from
// the pinned attribute package: where the key is, and which functions take
// none.
func TestAttributeKeyFuncs(t *testing.T) {
	funcs, err := attributeKeyFuncs()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"String", "Int", "Int64", "Bool", "Float64", "Stringer", "StringSlice", "IntSlice", "Int64Slice", "BoolSlice", "Float64Slice"} {
		if got, ok := funcs[name]; !ok || got != (keyArgs{from: 0}) {
			t.Errorf("attribute.%s: got %+v (found %v), want the first argument only", name, got, ok)
		}
	}
	for _, name := range []string{"NewAllowKeysFilter", "NewDenyKeysFilter"} {
		if got, ok := funcs[name]; !ok || got != (keyArgs{from: 0, rest: true}) {
			t.Errorf("attribute.%s: got %+v (found %v), want every argument", name, got, ok)
		}
	}
	// Their string parameter is a value, or they take no string at all.
	for _, name := range []string{"StringValue", "NewSet", "BoolValue", "DefaultEncoder"} {
		if got, ok := funcs[name]; ok {
			t.Errorf("attribute.%s takes no key, got %+v", name, got)
		}
	}
}

// TestSDKOwnedKeys pins the convention the allow-list is read by: the keys in
// the first cell of a table row whose last cell begins with "SDK-owned".
func TestSDKOwnedKeys(t *testing.T) {
	doc := filepath.Join(t.TempDir(), "semconv.md")
	body := strings.Join([]string{
		"# Catalog",
		"",
		"| Key | Type | Notes |",
		"|---|---|---|",
		"| `cassandra.query.attempt` | int | SDK-owned. Driver-side attempt index. |",
		"| `db.thing.one`, `db.thing.two` | `x` wrapper | SDK-owned names for a thing. |",
		"| `http.owned.bold` | int | **SDK-owned (facade)** — set at Close. |",
		"| `object_store.*` | string | SDK-owned namespace. |",
		"| `http.client.request.duration` | Float64Histogram | Emitted by the SDK-owned `resty` wrapper. |",
		"| `db.operation.name` | string | Low-cardinality operation name. |",
		"| `messaging.with.pipe` | string | SDK-owned \\| escaped pipe in the last cell. |",
		"A line that says `db.prose.key` is SDK-owned but is not a table row.",
		"",
	}, "\n")
	if err := os.WriteFile(doc, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	owned, err := sdkOwnedKeys(doc)
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for key := range owned {
		got = append(got, key)
	}
	slices.Sort(got)
	want := []string{
		"cassandra.query.attempt",
		"db.thing.one",
		"db.thing.two",
		"http.owned.bold",
		"messaging.with.pipe",
		"object_store.*",
	}
	if !slices.Equal(got, want) {
		t.Errorf("sdkOwnedKeys = %q, want %q", got, want)
	}
}

// TestSDKOwnedKeys_FailsWhenTheConventionIsNotFound pins that a catalog with no
// marked row is an error. An empty allow-list is what a reworded table would
// produce, and the scan must not run against it as if it were a finding about
// the code.
func TestSDKOwnedKeys_FailsWhenTheConventionIsNotFound(t *testing.T) {
	doc := filepath.Join(t.TempDir(), "semconv.md")
	if err := os.WriteFile(doc, []byte("| `db.operation.name` | string | Operation name. |\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := sdkOwnedKeys(doc); err == nil {
		t.Error("sdkOwnedKeys returned no error for a catalog that marks nothing")
	}
	if _, err := sdkOwnedKeys(filepath.Join(t.TempDir(), "missing.md")); err == nil {
		t.Error("sdkOwnedKeys returned no error for a missing catalog")
	}
}

// TestSDKOwnedKeys_ReadsTheRealCatalog pins the convention against the file the
// gate actually reads: the one SDK-owned key the tree writes inside a semconv
// namespace is marked, and keys that are not SDK-owned are not picked up by a
// stray mention of the phrase.
func TestSDKOwnedKeys_ReadsTheRealCatalog(t *testing.T) {
	owned, err := sdkOwnedKeys(filepath.Join("..", catalogPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"cassandra.query.attempt", "gin.error.type", "redis.error.kind", "resty.error.kind", "minio.error.kind"} {
		if !owned[key] {
			t.Errorf("the catalog does not mark %q as SDK-owned", key)
		}
	}
	for _, key := range []string{"http.client.request.duration", "db.statement", "db.system", "db.operation.name", "pyroscope.profile.id"} {
		if owned[key] {
			t.Errorf("the catalog marks %q as SDK-owned, which it is not", key)
		}
	}
}

// testCatalog is a small catalog with the pinned attribute package's real key
// positions, so the scan tests exercise the functions the gate reads.
func testCatalog(t *testing.T) catalog {
	t.Helper()
	keyFuncs, err := attributeKeyFuncs()
	if err != nil {
		t.Fatal(err)
	}
	return catalog{
		keys: map[string]bool{
			"server.address":             true,
			"service.name":               true,
			"service.namespace":          true,
			"db.system.name":             true,
			"db.query.text":              true,
			"cassandra.page.size":        true,
			"messaging.destination.name": true,
		},
		owned:    map[string]bool{"cassandra.query.attempt": true, "db.owned.*": true},
		keyFuncs: keyFuncs,
	}
}

// scanTree writes files under a temporary root and returns the scan's findings
// as "relative/path.go:line: message", sorted. The column is dropped: it is not
// part of what the gate promises, and it moves with the fixture's indentation.
func scanTree(t *testing.T, cat catalog, files map[string]string) []string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	findings, err := scan(root, cat)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range findings {
		rel := filepath.ToSlash(strings.TrimPrefix(f, root+string(filepath.Separator)))
		position, message, ok := strings.Cut(rel, ": ")
		if !ok {
			t.Fatalf("unexpected finding %q", f)
		}
		got = append(got, position[:strings.LastIndex(position, ":")]+": "+message)
	}
	slices.Sort(got)
	return got
}

func assertFindings(t *testing.T, got, want []string) {
	t.Helper()
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("findings:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// TestScan pins Rule 1 and what it leaves alone: a literal equal to a pinned
// key is reported wherever it stands, outside test files, generated files,
// imports, the root examples directory, testdata and hidden directories.
func TestScan(t *testing.T) {
	got := scanTree(t, testCatalog(t), map[string]string{
		"pkg/flagged.go":        "package pkg\n\nvar a = \"server.address\"\nvar b = `service.name`\nvar c = \"app.order.id\"\n",
		"pkg/flagged_test.go":   "package pkg\n\nvar d = \"server.address\"\n",
		"pkg/gen.go":            "// Code generated by x; DO NOT EDIT.\n\npackage pkg\n\nvar e = \"server.address\"\n",
		"pkg/imports.go":        "package pkg\n\nimport _ \"server.address\"\n",
		"examples/app/main.go":  "package main\n\nvar f = \"server.address\"\n",
		"pkg/examples/inner.go": "package examples\n\nvar g = \"server.address\"\n",
		"pkg/testdata/x.go":     "package x\n\nvar h = \"server.address\"\n",
		".hidden/x.go":          "package x\n\nvar i = \"server.address\"\n",
	})

	assertFindings(t, got, []string{
		`pkg/examples/inner.go:3: "server.address" is a semconv attribute key`,
		`pkg/flagged.go:3: "server.address" is a semconv attribute key`,
		`pkg/flagged.go:4: "service.name" is a semconv attribute key`,
	})
}

// TestScan_RejectsAKeyThePinnedPackageLacks pins Rule 2 in each position an
// attribute key can stand in.
//
// The keys are the mistake Core Principle 5 describes: spellings of an earlier
// semconv version ("db.statement", "db.cassandra.table") and a typo. None is in
// the pinned set, so Rule 1 sees nothing; all are in a namespace the pinned
// package uses.
func TestScan_RejectsAKeyThePinnedPackageLacks(t *testing.T) {
	const unknown = ` is not an attribute key of go.opentelemetry.io/otel/semconv/v1.39.0, whose `
	got := scanTree(t, testCatalog(t), map[string]string{
		"pkg/positions.go": `package pkg

import "go.opentelemetry.io/otel/attribute"

const legacyTable attribute.Key = "db.cassandra.table"

var (
	conversion = attribute.Key("db.statement")
	stringKV   = attribute.String("db.sytem.name", "postgresql")
	intKV      = attribute.Int("cassandra.page_size", 10)
	boolKV     = attribute.Bool("messaging.destination.temp", true)
	sliceKV    = attribute.StringSlice("db.collections", nil)
	filter     = attribute.NewAllowKeysFilter("server.addr", "db.operation")
	literal    = attribute.KeyValue{Key: "db.user"}
	elided     = []attribute.KeyValue{{Key: "db.jdbc.driver_classname"}, {"db.mssql.instance_name", attribute.StringValue("x")}}
	list       = []attribute.Key{"service.instance", "db.name"}
	set        = map[attribute.Key]struct{}{"db.connection_string": {}}
	byName     = map[string]attribute.Key{"x": "db.redis.database_index"}
)
`,
	})

	assertFindings(t, got, []string{
		`pkg/positions.go:5: "db.cassandra.table"` + unknown + `"db" namespace it is in`,
		`pkg/positions.go:8: "db.statement"` + unknown + `"db" namespace it is in`,
		`pkg/positions.go:9: "db.sytem.name"` + unknown + `"db" namespace it is in`,
		`pkg/positions.go:10: "cassandra.page_size"` + unknown + `"cassandra" namespace it is in`,
		`pkg/positions.go:11: "messaging.destination.temp"` + unknown + `"messaging" namespace it is in`,
		`pkg/positions.go:12: "db.collections"` + unknown + `"db" namespace it is in`,
		`pkg/positions.go:13: "server.addr"` + unknown + `"server" namespace it is in`,
		`pkg/positions.go:13: "db.operation"` + unknown + `"db" namespace it is in`,
		`pkg/positions.go:14: "db.user"` + unknown + `"db" namespace it is in`,
		`pkg/positions.go:15: "db.jdbc.driver_classname"` + unknown + `"db" namespace it is in`,
		`pkg/positions.go:15: "db.mssql.instance_name"` + unknown + `"db" namespace it is in`,
		`pkg/positions.go:16: "service.instance"` + unknown + `"service" namespace it is in`,
		`pkg/positions.go:16: "db.name"` + unknown + `"db" namespace it is in`,
		`pkg/positions.go:17: "db.connection_string"` + unknown + `"db" namespace it is in`,
		`pkg/positions.go:18: "db.redis.database_index"` + unknown + `"db" namespace it is in`,
	})
}

// TestScan_ResolvesAKeyThatIsNotWrittenAsALiteral pins that Rule 2 follows a
// key through the forms that hide it from a literal comparison: a constant, a
// constant declared in another file of the package, a concatenation, and an
// import alias for the attribute package. It also pins the assembled form of a
// pinned key, which Rule 1 cannot see because no literal equals it — whether it
// is put together at the call or in a constant the call names.
func TestScan_ResolvesAKeyThatIsNotWrittenAsALiteral(t *testing.T) {
	const unknown = ` is not an attribute key of go.opentelemetry.io/otel/semconv/v1.39.0, whose "db" namespace it is in`
	got := scanTree(t, testCatalog(t), map[string]string{
		"pkg/consts.go": `package pkg

const (
	statementKey = "db.statement"
	prefix       = "db."
	// A pinned key that no literal equals: Rule 1 sees neither half.
	assembledKey = "service." + "namespace"
	viaAssembled = assembledKey
)
`,
		"pkg/uses.go": `package pkg

import attr "go.opentelemetry.io/otel/attribute"

var (
	viaConst   = attr.String(statementKey, "SELECT 1")
	viaConcat  = attr.String("db."+"statement", "SELECT 1")
	viaPrefix  = attr.Key(prefix + "instance")
	viaConvert = attr.Key(string(statementKey))
	assembled  = attr.String("service."+"namespace", "platform")
	inConst    = attr.String(assembledKey, "platform")
	twoSteps   = attr.Key(viaAssembled)
)
`,
	})

	assertFindings(t, got, []string{
		`pkg/uses.go:6: "db.statement"` + unknown,
		`pkg/uses.go:7: "db.statement"` + unknown,
		`pkg/uses.go:8: "db.instance"` + unknown,
		`pkg/uses.go:9: "db.statement"` + unknown,
		`pkg/uses.go:10: "service.namespace" is a semconv attribute key, assembled from parts`,
		`pkg/uses.go:11: "service.namespace" is a semconv attribute key, assembled from parts`,
		`pkg/uses.go:12: "service.namespace" is a semconv attribute key, assembled from parts`,
	})
}

// TestScan_AcceptsWhatRule2IsNotAbout pins that Rule 2 stays out of everything
// that is not an unknown key in a key position:
//
//   - an SDK-owned key, by name and under an owned prefix;
//   - a key in a namespace the pinned package does not use, and one with no
//     namespace at all;
//   - strings that read like keys but are not in a key position: a metric
//     name, a span name, a log message, an attribute value;
//   - a pinned key named through its constant;
//   - a key the scan cannot resolve: a function result, a variable (it may be
//     assigned again), a name declared twice or shadowed by a parameter or a :=
//     variable, a lookup method named Key;
//   - a call on a local that has the attribute package's name;
//   - a function of the same name in a package that is not the attribute one;
//   - a test file.
func TestScan_AcceptsWhatRule2IsNotAbout(t *testing.T) {
	got := scanTree(t, testCatalog(t), map[string]string{
		"pkg/ok.go": `package pkg

import (
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"

	other "example.com/app/attribute"
)

const metricName = "db.client.operation.duration"

func name() string { return "db.statement" }

func f(meter interface{ Float64Histogram(string) }, tracer interface{ Start(string) }) {
	_ = attribute.Key("cassandra.query.attempt")
	_ = attribute.String("db.owned.anything", "x")
	_ = attribute.String("object_store.bucket.name", "b")
	_ = attribute.String("redis.error.kind", "pool_timeout")
	_ = attribute.String("tenant", "acme")
	_ = attribute.String("app.db.statement", "x")
	meter.Float64Histogram("db.client.operation.duration")
	meter.Float64Histogram(metricName)
	tracer.Start("db.statement")
	println("db.statement was renamed to db.query.text")
	_ = attribute.String("app.note", "db.statement")
	_ = semconv.DBSystemNameKey
	_ = attribute.NewAllowKeysFilter(semconv.DBSystemNameKey, semconv.ServerAddressKey)
	_ = attribute.String(name(), "x")
	_ = other.String("db.statement", "x")
	_ = other.Key("db.statement")
}

func g() {
	key := "db.statement"
	_ = key
}

func h() {
	key := "app.thing"
	_ = attribute.String(key, "x")
}

// A package constant and a parameter of the same name: the call below uses the
// parameter, so the constant's text says nothing about the key.
const label = "db.statement was renamed"

const dotted = "db.statement"

func shadowedByParam(dotted string) attribute.KeyValue { return attribute.String(dotted, "x") }

func shadowedByDefine() attribute.KeyValue {
	label := "app.label"
	return attribute.String(label, "x")
}

type lookup struct{}

func (lookup) Key(string) string { return "app.resolved" }

// Not a conversion: cfg.Key looks a key up, it does not hold one.
func viaLookup(cfg lookup) attribute.KeyValue { return attribute.String(cfg.Key("db.statement"), "x") }

// A variable is not resolved: by the time it is used it holds something else.
var reassigned = "db.statement"

func viaReassigned() attribute.KeyValue {
	reassigned = "app.query"
	return attribute.String(reassigned, "x")
}
`,
		"pkg/ok_test.go": `package pkg

import "go.opentelemetry.io/otel/attribute"

var legacy = attribute.Key("db.statement")
`,
		// The file imports the attribute package and also has a parameter of
		// that name. Inside the function the name is the parameter, so the
		// call builds no attribute.
		"pkg/shadowed.go": `package pkg

import "go.opentelemetry.io/otel/attribute"

var _ = attribute.Bool("app.flag", true)

type recorder struct{}

func (recorder) String(string, string) {}

func record(attribute recorder) { attribute.String("db.statement", "ok") }
`,
		"pkg/noimport.go": `package pkg

type fake struct{}

func (fake) Key(string) string { return "" }

var attribute fake

var notTheAttributePackage = attribute.Key("db.statement")
`,
	})

	if len(got) != 0 {
		t.Errorf("nothing here is an unknown key in a key position, got:\n  %s", strings.Join(got, "\n  "))
	}
}

// TestRepositoryIsClean runs the gate's own scan over the repository with the
// real catalog. `make semconv-check` runs the same scan as a command; having it
// here as well means a test run reports the finding next to the tests that
// explain the rule.
func TestRepositoryIsClean(t *testing.T) {
	cat, err := loadCatalog(filepath.Join("..", catalogPath))
	if err != nil {
		t.Fatal(err)
	}
	findings, err := scan("..", cat)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Error(f)
	}
}
