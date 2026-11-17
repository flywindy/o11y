//go:build ignore

package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var integrationDirs = []string{"http", "nats", "mongo", "gin", "resty", "redis", "minio", "elasticsearch", "cassandra"}

// globalSetters lists the functions that replace a process-wide OpenTelemetry
// value, by the import path of the package that declares them. SDK code may
// call none of them (ADR 0003).
//
// The key is the package, not only the name. An earlier form of this gate held
// one set of names and matched them against go.opentelemetry.io/otel alone, so
// its SetLoggerProvider entry could never fire: the logs global lives in
// go.opentelemetry.io/otel/log/global. SetErrorHandler and SetLogger, which
// are in the root package and are process-wide like the rest, were not listed
// at all. TestGlobalSettersExist reads each function out of the pinned module
// so an entry that names nothing fails instead of sitting here unnoticed.
var globalSetters = map[string][]string{
	"go.opentelemetry.io/otel": {
		"SetTracerProvider",
		"SetTextMapPropagator",
		"SetMeterProvider",
		"SetErrorHandler",
		"SetLogger",
	},
	"go.opentelemetry.io/otel/log/global": {
		"SetLoggerProvider",
	},
}

// examplesDir holds the runnable examples at the repository root. They are
// application code, not SDK code.
const examplesDir = "examples"

// isGlobalSetter reports whether name, declared in the package at pkgPath, is
// one of globalSetters.
func isGlobalSetter(pkgPath, name string) bool {
	return slices.Contains(globalSetters[pkgPath], name)
}

func main() {
	var errs []error
	errs = append(errs, checkGoModIntegrations()...)
	errs = append(errs, checkTierAnnotations()...)
	errs = append(errs, checkNoGlobalSetters(".")...)
	errs = append(errs, checkRootDoesNotLinkDrivers()...)
	if len(errs) > 0 {
		for _, err := range errs {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
}

func checkGoModIntegrations() []error {
	modules, err := instrumentationModules("go.mod")
	if err != nil {
		return []error{err}
	}
	adr, err := os.ReadFile(filepath.Join("docs", "adr", "0003-global-state-policy.md"))
	if err != nil {
		return []error{fmt.Errorf("read ADR 0003: %w", err)}
	}
	var errs []error
	for _, module := range modules {
		needle := module
		if strings.HasPrefix(module, "github.com/") {
			needle = strings.TrimPrefix(module, "github.com/")
		}
		if !bytes.Contains(adr, []byte(module)) && !bytes.Contains(adr, []byte(needle)) {
			errs = append(errs, fmt.Errorf("ADR 0003 missing approved integration row for %s", module))
		}
	}
	return errs
}

func instrumentationModules(path string) ([]string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read go.mod: %w", err)
	}
	var modules []string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") || line == "require (" || line == ")" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		module := fields[0]
		if strings.HasPrefix(module, "go.opentelemetry.io/contrib/instrumentation/") ||
			strings.HasPrefix(module, "github.com/akira-core/instrumentation-go/") ||
			module == "github.com/minio/minio-go/v7" ||
			module == "github.com/elastic/go-elasticsearch/v8" ||
			module == "github.com/elastic/elastic-transport-go/v8" ||
			module == "github.com/grafana/pyroscope-go" ||
			module == "github.com/grafana/otel-profiling-go" {
			modules = append(modules, module)
		}
	}
	return modules, nil
}

func checkTierAnnotations() []error {
	var errs []error
	for _, dir := range integrationDirs {
		info, err := os.Stat(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("stat %s: %w", dir, err))
			continue
		}
		if !info.IsDir() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, "doc.go"))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s/doc.go missing tier annotation: %w", dir, err))
			continue
		}
		text := string(body)
		hasT2 := strings.Contains(text, "// Tier: T2")
		hasT3 := strings.Contains(text, "// Tier: T3")
		if !hasT2 && !hasT3 {
			errs = append(errs, fmt.Errorf("%s/doc.go missing // Tier: T2 or // Tier: T3 annotation", dir))
		}
		if hasT3 && !adrMentionsPackage(dir) {
			errs = append(errs, fmt.Errorf("%s is T3 but no ADR mentions the package path", dir))
		}
	}
	return errs
}

func adrMentionsPackage(dir string) bool {
	matches, err := filepath.Glob(filepath.Join("docs", "adr", "*.md"))
	if err != nil {
		return false
	}
	pattern := regexp.MustCompile(`\b` + regexp.QuoteMeta(dir) + "(?:/|[\\s\"'`)\\]]|$)")
	for _, match := range matches {
		body, err := os.ReadFile(match)
		if err == nil && pattern.Match(body) {
			return true
		}
	}
	return false
}

// checkNoGlobalSetters reports every use of a globalSetters function in a
// non-test Go file under root.
//
// A use is any identifier that resolves to one of those functions, however it
// is written: otel.SetTracerProvider(tp), the same through an import alias or
// a dot import, and a reference that is not a call (f := otel.SetLogger). The
// identifier is resolved through go/types rather than matched as text, so a
// function of the same name in another package, a method, and a comment or
// string that mentions one are not reported.
//
// Two things are exempt. Test files: a test may install a global to prove the
// SDK does not read it. And the examples directory at the root: its programs
// are applications, and ADR 0003 leaves an application's main free to set
// globals — the README's recommended wiring is
// otel.SetErrorHandler(obs.ErrorHandler()) and otel.SetLogger(obs.Logr()), so
// an example that follows it must not fail the gate. Only the root directory
// of that name is exempt; a package elsewhere that happens to be called
// examples is SDK code like any other. The semconv literal gate draws the same
// line.
func checkNoGlobalSetters(root string) []error {
	var errs []error
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			errs = append(errs, fmt.Errorf("walk %s: %w", path, err))
			return nil
		}
		if d.IsDir() && (shouldSkipDir(d.Name()) || path == filepath.Join(root, examplesDir)) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			errs = append(errs, fmt.Errorf("parse %s: %w", path, err))
			return nil
		}
		// Uses maps every identifier to what it denotes. For a selector such
		// as otel.SetTracerProvider that is the Sel identifier, and for a dot
		// import it is the bare name, so one pass over the map covers both —
		// for the expressions the type checker visits.
		found := map[*ast.Ident]string{}
		info := typeInfoForFile(fset, file)
		for ident, obj := range info.Uses {
			fn, ok := obj.(*types.Func)
			if !ok || fn.Pkg() == nil {
				continue
			}
			if isGlobalSetter(fn.Pkg().Path(), fn.Name()) {
				found[ident] = fn.Pkg().Path()
			}
		}
		// It does not visit them all. Checked one file at a time against
		// empty packages, much of a file fails to type-check, and go/types
		// gives up on some expressions without looking inside: a map literal
		// whose key it cannot evaluate — strings.ToLower("k"), or a constant
		// from another file of the package — has its value skipped, so
		// otel.SetErrorHandler written there never reaches Uses.
		// unvisitedSetters reports those from the syntax alone.
		for ident, pkgPath := range unvisitedSetters(file, info) {
			found[ident] = pkgPath
		}
		idents := slices.Collect(maps.Keys(found))
		// Map order is random; report in source order.
		slices.SortFunc(idents, func(a, b *ast.Ident) int { return int(a.Pos() - b.Pos()) })
		for _, ident := range idents {
			errs = append(errs, fmt.Errorf(
				"%s: forbidden %s.%s: SDK code must not set an OpenTelemetry global (ADR 0003)",
				fset.Position(ident.Pos()), found[ident], ident.Name,
			))
		}
		return nil
	})
	if err != nil {
		errs = append(errs, err)
	}
	return errs
}

// unvisitedSetters finds setter references in the parts of file the type
// checker did not visit, from the syntax tree and the file's imports alone.
//
// An identifier the type checker reached is in info.Uses or info.Defs, and the
// main pass has already judged it; only the rest is looked at here. Without
// type information a name is matched to an import by its spelling, so a name
// bound in a scope that encloses the reference — a parameter called otel, a
// local function value called SetLogger — is left alone, since there it means
// the local and not the import. A binding in some other scope of the file does
// not count. Some names are not references at all and are never reported: a
// field key in a composite literal (Hooks{SetLogger: …}), a name being
// declared (a struct field, interface method, parameter, var, const, type, :=
// or range variable), and a label, which lives in a namespace of its own.
func unvisitedSetters(file *ast.File, info *types.Info) map[*ast.Ident]string {
	imported := map[string]string{} // local name -> import path
	var dotImported []string
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || len(globalSetters[path]) == 0 {
			continue
		}
		switch {
		case spec.Name == nil:
			imported[packageName(path)] = path
		case spec.Name.Name == ".":
			dotImported = append(dotImported, path)
		case spec.Name.Name != "_":
			imported[spec.Name.Name] = path
		}
	}
	if len(imported) == 0 && len(dotImported) == 0 {
		return nil
	}
	scopes := fileScopes(file)
	visited := func(ident *ast.Ident) bool {
		_, used := info.Uses[ident]
		_, defined := info.Defs[ident]
		return used || defined
	}

	found := map[*ast.Ident]string{}
	skip := map[*ast.Ident]bool{} // selector halves and declared names
	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.ImportSpec:
			return false
		case *ast.SelectorExpr:
			skip[n.Sel] = true
			x, ok := n.X.(*ast.Ident)
			if !ok {
				return true
			}
			skip[x] = true
			if visited(x) || scopes.binds(x.Name, x.Pos()) {
				return true
			}
			if path, ok := imported[x.Name]; ok && isGlobalSetter(path, n.Sel.Name) {
				found[n.Sel] = path
			}
		case *ast.CompositeLit:
			for _, elt := range n.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					if key, ok := kv.Key.(*ast.Ident); ok {
						skip[key] = true
					}
				}
			}
		case *ast.Field:
			// Struct fields, interface methods, parameters and results.
			skipAll(skip, n.Names...)
		case *ast.ValueSpec:
			skipAll(skip, n.Names...)
		case *ast.TypeSpec:
			skipAll(skip, n.Name)
		case *ast.AssignStmt:
			if n.Tok == token.DEFINE {
				skipAll(skip, identsOf(n.Lhs...)...)
			}
		case *ast.RangeStmt:
			if n.Tok == token.DEFINE {
				skipAll(skip, identsOf(n.Key, n.Value)...)
			}
		case *ast.LabeledStmt:
			skipAll(skip, n.Label)
		case *ast.BranchStmt:
			skipAll(skip, n.Label)
		case *ast.Ident:
			if skip[n] || visited(n) || scopes.binds(n.Name, n.Pos()) {
				return true
			}
			for _, path := range dotImported {
				if isGlobalSetter(path, n.Name) {
					found[n] = path
				}
			}
		}
		return true
	})
	return found
}

// scope is a stretch of source in which a set of names is bound.
type scope struct {
	from, to token.Pos
	names    map[string]bool
}

// scopes are the bindings of one file, each with the stretch of source it
// covers.
type scopes []scope

// binds reports whether name is bound by some binding whose scope contains
// pos.
func (s scopes) binds(name string, pos token.Pos) bool {
	for _, sc := range s {
		if sc.names[name] && sc.from <= pos && pos < sc.to {
			return true
		}
	}
	return false
}

// fileScopes collects the bindings of file with the scope Go gives each.
//
// A package-level declaration covers the file. A receiver, parameter or result
// covers its function. A local var, const, type or := starts after its own
// declaration — in "otel := otel.X", the right-hand side still means the
// import — and runs to the end of the enclosing block. A variable declared in
// the header of an if, for, switch, type switch or range statement, or in a
// select case, covers that statement or case. Labels and struct fields do not
// bind ordinary names and are left out.
func fileScopes(file *ast.File) scopes {
	var out scopes
	add := func(from, to token.Pos, idents ...*ast.Ident) {
		names := map[string]bool{}
		for _, ident := range idents {
			if ident != nil && ident.Name != "_" {
				names[ident.Name] = true
			}
		}
		if len(names) > 0 {
			out = append(out, scope{from: from, to: to, names: names})
		}
	}
	fieldNames := func(lists ...*ast.FieldList) []*ast.Ident {
		var ids []*ast.Ident
		for _, list := range lists {
			if list != nil {
				for _, field := range list.List {
					ids = append(ids, field.Names...)
				}
			}
		}
		return ids
	}
	specNames := func(decl *ast.GenDecl) []*ast.Ident {
		var ids []*ast.Ident
		for _, spec := range decl.Specs {
			switch sp := spec.(type) {
			case *ast.ValueSpec:
				ids = append(ids, sp.Names...)
			case *ast.TypeSpec:
				ids = append(ids, sp.Name)
			}
		}
		return ids
	}
	// defined returns the variables an init statement declares with :=.
	defined := func(stmt ast.Stmt) []*ast.Ident {
		if assign, ok := stmt.(*ast.AssignStmt); ok && assign.Tok == token.DEFINE {
			return identsOf(assign.Lhs...)
		}
		return nil
	}

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			if d.Tok != token.IMPORT {
				add(file.Pos(), file.End(), specNames(d)...)
			}
		case *ast.FuncDecl:
			if d.Recv == nil {
				add(file.Pos(), file.End(), d.Name)
			}
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.FuncDecl:
			add(n.Type.Pos(), n.End(), fieldNames(n.Recv, n.Type.Params, n.Type.Results)...)
		case *ast.FuncLit:
			add(n.Pos(), n.End(), fieldNames(n.Type.Params, n.Type.Results)...)
		case *ast.BlockStmt:
			for _, stmt := range n.List {
				switch st := stmt.(type) {
				case *ast.DeclStmt:
					if gen, ok := st.Decl.(*ast.GenDecl); ok {
						add(st.End(), n.End(), specNames(gen)...)
					}
				case *ast.AssignStmt:
					add(st.End(), n.End(), defined(st)...)
				}
			}
		case *ast.IfStmt:
			if n.Init != nil {
				add(n.Init.End(), n.End(), defined(n.Init)...)
			}
		case *ast.ForStmt:
			if n.Init != nil {
				add(n.Init.End(), n.End(), defined(n.Init)...)
			}
		case *ast.SwitchStmt:
			if n.Init != nil {
				add(n.Init.End(), n.End(), defined(n.Init)...)
			}
		case *ast.TypeSwitchStmt:
			if n.Init != nil {
				add(n.Init.End(), n.End(), defined(n.Init)...)
			}
			add(n.Assign.End(), n.End(), defined(n.Assign)...)
		case *ast.RangeStmt:
			if n.Tok == token.DEFINE {
				add(n.X.End(), n.End(), identsOf(n.Key, n.Value)...)
			}
		case *ast.CommClause:
			if n.Comm != nil {
				add(n.Comm.End(), n.End(), defined(n.Comm)...)
			}
		}
		return true
	})
	return out
}

// identsOf returns the expressions among exprs that are plain identifiers.
func identsOf(exprs ...ast.Expr) []*ast.Ident {
	var ids []*ast.Ident
	for _, expr := range exprs {
		if ident, ok := expr.(*ast.Ident); ok {
			ids = append(ids, ident)
		}
	}
	return ids
}

// skipAll marks each non-nil ident in skip.
func skipAll(skip map[*ast.Ident]bool, idents ...*ast.Ident) {
	for _, ident := range idents {
		if ident != nil {
			skip[ident] = true
		}
	}
}

// typeInfoForFile type-checks one file on its own, far enough to say what each
// identifier denotes. Errors are discarded: the file is checked without the
// rest of its package and against policyImporter's empty packages, so most of
// it does not type-check, and nothing here needs it to.
func typeInfoForFile(fset *token.FileSet, file *ast.File) *types.Info {
	info := &types.Info{
		Uses: make(map[*ast.Ident]types.Object),
		Defs: make(map[*ast.Ident]types.Object),
	}
	conf := types.Config{
		Importer: policyImporter{},
		Error:    func(error) {},
	}
	_, _ = conf.Check(file.Name.Name, fset, []*ast.File{file}, info)
	return info
}

// policyImporter resolves every import to an empty package, except that the
// packages in globalSetters declare those functions. That is all the gate needs
// in order to tell go.opentelemetry.io/otel.SetLogger from a SetLogger declared
// anywhere else, and it means the scan needs neither a module cache nor a
// build.
type policyImporter struct{}

func (policyImporter) Import(path string) (*types.Package, error) {
	pkg := types.NewPackage(path, packageName(path))
	empty := types.NewTuple()
	sig := types.NewSignatureType(nil, nil, nil, empty, empty, false)
	for _, name := range globalSetters[path] {
		pkg.Scope().Insert(types.NewFunc(token.NoPos, pkg, name, sig))
	}
	pkg.MarkComplete()
	return pkg, nil
}

func packageName(importPath string) string {
	name := importPath
	if idx := strings.LastIndex(importPath, "/"); idx >= 0 {
		name = importPath[idx+1:]
	}
	if name == "" {
		return "pkg"
	}
	for _, r := range name {
		if r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			continue
		}
		return "pkg"
	}
	return name
}

func shouldSkipDir(name string) bool {
	switch name {
	case ".git", ".gocache", ".gomodcache", ".gotmp", "node_modules", "vendor", "dist":
		return true
	default:
		return false
	}
}

// rootForbiddenDepPrefixes are module paths the root o11y package must never
// link. The root package composes every integration's metric views into
// o11y.Init; a view that lives beside its driver would make that driver a
// compile-time dependency of every consumer of the root package, even one that
// never uses the integration (ADR 0026 Option A). Every integration's views
// live in the driver-free internal/views package, and this check keeps them
// there — an unenforced convention is how the original coupling accumulated.
var rootForbiddenDepPrefixes = []string{
	"github.com/elastic/",
	"github.com/gocql/",
	"github.com/minio/",
	"github.com/redis/",
	"go.mongodb.org/",
}

// checkRootDoesNotLinkDrivers asserts, via the build graph rather than source
// imports, that the root package's transitive dependencies contain none of
// rootForbiddenDepPrefixes.
func checkRootDoesNotLinkDrivers() []error {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".").Output()
	if err != nil {
		return []error{fmt.Errorf("go list -deps .: %w", err)}
	}
	var errs []error
	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		for _, prefix := range rootForbiddenDepPrefixes {
			if strings.HasPrefix(dep, prefix) {
				errs = append(errs, fmt.Errorf("root package links %s: integration drivers must stay out of the root package's dependency graph (ADR 0026 Option A / ADR 0027 §5)", dep))
			}
		}
	}
	return errs
}
