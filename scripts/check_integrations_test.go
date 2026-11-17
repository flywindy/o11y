//go:build ignore

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// writeTree writes files, keyed by slash-separated path, under a new temporary
// directory and returns it.
func writeTree(t *testing.T, files map[string]string) string {
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
	return root
}

// findings runs the gate over root and returns each report as
// "relative/path.go:line: import/path.Func", sorted.
func findings(t *testing.T, root string) []string {
	t.Helper()
	var got []string
	for _, err := range checkNoGlobalSetters(root) {
		text := strings.TrimPrefix(err.Error(), root+string(filepath.Separator))
		position, rest, ok := strings.Cut(text, ": forbidden ")
		if !ok {
			t.Fatalf("unexpected report %q", err)
		}
		// position is file:line:column; the column is not part of the contract.
		position = position[:strings.LastIndex(position, ":")]
		setter, _, _ := strings.Cut(rest, ":")
		got = append(got, filepath.ToSlash(position)+": "+setter)
	}
	slices.Sort(got)
	return got
}

// TestCheckNoGlobalSetters_ReportsEachSetter pins that every function in
// globalSetters is reported when SDK code calls it, one file per function so a
// miss names the function.
//
// The gate used to match one set of names against go.opentelemetry.io/otel
// alone. A file calling log/global.SetLoggerProvider, otel.SetErrorHandler and
// otel.SetLogger passed it, and only otel.SetTracerProvider was reported.
func TestCheckNoGlobalSetters_ReportsEachSetter(t *testing.T) {
	for pkgPath, names := range globalSetters {
		for _, name := range names {
			t.Run(pkgPath+"."+name, func(t *testing.T) {
				root := writeTree(t, map[string]string{
					"internal/x/x.go": "package x\n\nimport pkg \"" + pkgPath + "\"\n\nfunc f() {\n\tpkg." + name + "(nil)\n}\n",
				})

				got := findings(t, root)

				want := []string{"internal/x/x.go:6: " + pkgPath + "." + name}
				if !slices.Equal(got, want) {
					t.Errorf("got %q, want %q", got, want)
				}
			})
		}
	}
}

// TestCheckNoGlobalSetters_ReportsEveryWayOfWritingOne pins that the gate
// follows the identifier to its package rather than matching text: the default
// import name, an alias, a dot import, a reference that is not a call, and
// several in one file.
func TestCheckNoGlobalSetters_ReportsEveryWayOfWritingOne(t *testing.T) {
	root := writeTree(t, map[string]string{
		"plain.go": `package x

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log/global"
)

func f() {
	otel.SetTracerProvider(nil)
	global.SetLoggerProvider(nil)
}
`,
		"alias.go": `package x

import (
	api "go.opentelemetry.io/otel"
	g "go.opentelemetry.io/otel/log/global"
)

func f() {
	g.SetLoggerProvider(nil)
	api.SetErrorHandler(nil)
}
`,
		"dot.go": `package x

import (
	. "go.opentelemetry.io/otel"
	. "go.opentelemetry.io/otel/log/global"
)

func f() {
	SetLogger(nil)
	SetLoggerProvider(nil)
}
`,
		"reference.go": `package x

import "go.opentelemetry.io/otel"

var install = otel.SetMeterProvider

func f() { defer otel.SetTextMapPropagator(nil) }
`,
	})

	got := findings(t, root)

	want := []string{
		"alias.go:9: go.opentelemetry.io/otel/log/global.SetLoggerProvider",
		"alias.go:10: go.opentelemetry.io/otel.SetErrorHandler",
		"dot.go:9: go.opentelemetry.io/otel.SetLogger",
		"dot.go:10: go.opentelemetry.io/otel/log/global.SetLoggerProvider",
		"plain.go:9: go.opentelemetry.io/otel.SetTracerProvider",
		"plain.go:10: go.opentelemetry.io/otel/log/global.SetLoggerProvider",
		"reference.go:5: go.opentelemetry.io/otel.SetMeterProvider",
		"reference.go:7: go.opentelemetry.io/otel.SetTextMapPropagator",
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("got:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// TestCheckNoGlobalSetters_IgnoresWhatIsNotASetter pins the other side. Each
// file holds something that reads like a forbidden call and is not one:
//
//   - a function of the same name in another package, including a package that
//     is also called "global" or "otel";
//   - a setter name that belongs to the other OpenTelemetry package — the logs
//     global is not in the root package, and the tracer global is not in
//     log/global;
//   - a method, a local function and a local variable with a setter's name;
//   - the getters, which the gate does not cover;
//   - a comment and a string that mention one;
//   - a test file, and directories the gate skips;
//   - a program under the root examples directory, which is an application and
//     may set globals as the README shows.
func TestCheckNoGlobalSetters_IgnoresWhatIsNotASetter(t *testing.T) {
	root := writeTree(t, map[string]string{
		"otherpkg.go": `package x

import (
	global "example.com/app/global"
	otel "example.com/app/otel"
	"example.com/app/logging"
)

func f() {
	global.SetLoggerProvider(nil)
	otel.SetTracerProvider(nil)
	logging.SetLogger(nil)
}
`,
		"wrongpkg.go": `package x

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log/global"
)

func f() {
	otel.SetLoggerProvider(nil)
	global.SetTracerProvider(nil)
}
`,
		"local.go": `package x

import "go.opentelemetry.io/otel"

type registry struct{}

func (registry) SetTracerProvider(any) {}

func SetMeterProvider(any) {}

func f() {
	var r registry
	r.SetTracerProvider(nil)
	SetMeterProvider(nil)
	SetLogger := func(any) {}
	SetLogger(nil)
	_ = otel.GetTracerProvider()
	_ = otel.GetTextMapPropagator()
}
`,
		"shadow.go": `package x

type facade struct{}

func (facade) SetErrorHandler(any) {}

func f() {
	var otel facade
	otel.SetErrorHandler(nil)
}
`,
		"text.go": `package x

// f does not call otel.SetTracerProvider(tp).
func f() string { return "otel.SetLogger(l); global.SetLoggerProvider(p)" }
`,
		"x_test.go": `package x

import "go.opentelemetry.io/otel"

func g() { otel.SetTracerProvider(nil) }
`,
		"examples/app/main.go": `package main

import "go.opentelemetry.io/otel"

func main() {
	otel.SetErrorHandler(nil)
	otel.SetLogger(nil)
	otel.SetTracerProvider(nil)
}
`,
		"vendor/v/v.go": `package v

import "go.opentelemetry.io/otel"

func f() { otel.SetTracerProvider(nil) }
`,
		".git/hooks/h.go": `package h

import "go.opentelemetry.io/otel"

func f() { otel.SetTracerProvider(nil) }
`,
	})

	if got := findings(t, root); len(got) != 0 {
		t.Errorf("nothing here sets an OpenTelemetry global, got:\n  %s", strings.Join(got, "\n  "))
	}
}

// TestCheckNoGlobalSetters_ExemptsOnlyTheRootExamples pins where the examples
// exemption stops. The directory at the root holds applications. A directory
// of the same name deeper in the tree is a package of the SDK, and a setter
// there is reported like any other.
func TestCheckNoGlobalSetters_ExemptsOnlyTheRootExamples(t *testing.T) {
	const program = `package main

import "go.opentelemetry.io/otel"

func main() { otel.SetErrorHandler(nil) }
`
	root := writeTree(t, map[string]string{
		"examples/basic/main.go":      program,
		"examples/main.go":            program,
		"internal/examples/main.go":   program,
		"redis/examples/demo/main.go": program,
	})

	got := findings(t, root)

	want := []string{
		"internal/examples/main.go:5: go.opentelemetry.io/otel.SetErrorHandler",
		"redis/examples/demo/main.go:5: go.opentelemetry.io/otel.SetErrorHandler",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestGlobalSettersExist pins that every entry of globalSetters names a
// function the pinned OpenTelemetry modules actually export.
//
// An entry that names nothing is silent: the gate reports no call because no
// call can resolve to it. That is how SetLoggerProvider sat in the list under
// the root package, where it has never existed, while the real one in
// log/global went unchecked.
func TestGlobalSettersExist(t *testing.T) {
	for pkgPath, names := range globalSetters {
		out, err := exec.Command("go", "list", "-f", "{{.Dir}}", pkgPath).Output()
		if err != nil {
			t.Fatalf("go list %s: %v", pkgPath, err)
		}
		exported := exportedFuncs(t, strings.TrimSpace(string(out)))
		for _, name := range names {
			if !exported[name] {
				t.Errorf("%s does not export a function named %s", pkgPath, name)
			}
		}
	}
}

// exportedFuncs returns the names of the package-level functions declared in
// the non-test Go files of dir.
func exportedFuncs(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	funcs := make(map[string]bool)
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.IsExported() {
				funcs[fn.Name.Name] = true
			}
		}
	}
	return funcs
}

// adrSetterRow matches one row of the table in ADR 0003's Decision section:
// the package path, then the function as a reader would write the call.
var adrSetterRow = regexp.MustCompile("(?m)^\\| `([^`]+)` \\| `[A-Za-z]+\\.([A-Za-z]+)` \\|")

// TestADR0003NamesEveryGatedSetter pins that the policy and its enforcement
// list the same functions: the table in ADR 0003's Decision section has one
// row for each function the gate rejects, and no row for one it does not.
//
// The rows are compared, not the prose. The section also discusses these
// functions in running text, so a search for each name would still succeed
// after its row was deleted.
func TestADR0003NamesEveryGatedSetter(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "docs", "adr", "0003-global-state-policy.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(body), "\n## Decision\n")
	if !ok {
		t.Fatal("ADR 0003 has no Decision section")
	}
	decision, _, _ := strings.Cut(rest, "\n## ")

	var documented []string
	for _, row := range adrSetterRow.FindAllStringSubmatch(decision, -1) {
		documented = append(documented, row[1]+"."+row[2])
	}
	var gated []string
	for pkgPath, names := range globalSetters {
		for _, name := range names {
			gated = append(gated, pkgPath+"."+name)
		}
	}
	slices.Sort(documented)
	slices.Sort(gated)
	if !slices.Equal(documented, gated) {
		t.Errorf("ADR 0003's Decision table and the gate disagree.\n  ADR:  %q\n  gate: %q", documented, gated)
	}
}
