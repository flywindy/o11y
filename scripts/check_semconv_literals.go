//go:build ignore

// Command check_semconv_literals enforces docs/semconv.md Enforcement Rules 1
// and 2 on SDK code.
//
// Rule 1: a semantic-convention attribute key is named through the constant in
// the pinned semconv package, never as a string literal. The key set is read
// from the pinned package's own source, every attribute.Key("...") it declares,
// so it follows the go.mod pin with nothing to regenerate. A string literal in
// SDK code that equals one of those keys is reported with the line it is on.
//
// Rule 2: a key written where an attribute key goes, inside a namespace the
// pinned package uses, has to be a key the pinned package declares or one
// docs/semconv.md marks as SDK-owned. Rule 1 on its own is blind to exactly the
// mistake Core Principle 5 describes: "db.statement" and "db.cassandra.table"
// are keys of an earlier semconv version, so they are not in the pinned set
// and a literal holding one equals nothing. This rule sees them, because "db"
// is a namespace of the pinned package and neither key is in it.
//
// Rule 2 looks only at attribute-key positions, not at every string. A metric
// name ("db.client.operation.duration"), a span name and a log message all
// read like dotted keys and are none of its business. The positions are:
//
//	attribute.Key(k)                      the conversion
//	attribute.String(k, v) and the rest   every function of the pinned attribute
//	                                      package whose first parameter is the key
//	attribute.NewAllowKeysFilter(k...)    and NewDenyKeysFilter
//	const k attribute.Key = ...           a declaration typed attribute.Key
//	attribute.KeyValue{Key: k}            the composite literal
//	[]attribute.Key{k}, map[attribute.Key]T{k: v}
//
// In such a position the key may be a literal, a concatenation of literals, or
// the name of a string constant or variable declared in the same directory;
// all three are resolved. The functions are read from the pinned attribute
// package, as the keys are from the pinned semconv package.
//
// The SDK-owned keys are read from docs/semconv.md, not listed here: a table
// row whose last cell begins with "SDK-owned" marks the keys in its first cell.
// A key the SDK defines in a semconv namespace (cassandra.query.attempt) is
// documented there already, so the catalog is the allow-list and the two cannot
// drift apart.
//
// Out of scope, each for a reason:
//
//	_test.go files    a test pins the wire string on purpose, so a renamed
//	                  constant cannot silently change what the test checks
//	examples/         application code, not SDK-owned (the root examples/ only)
//	generated files   their generator decides what they hold (the baggage
//	                  catalog is the key set itself, as data)
//	hidden dirs, testdata, vendor
//
// What is still left to review: a key assembled at run time, one that comes
// from a constant declared in another package or from a name that is also a
// parameter or local variable somewhere in the directory, a method argument
// such as set.Value("k") whose receiver type only a type-checker could name, a
// file that dot-imports the attribute package, and a key in a namespace the
// pinned package does not use at all.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// semconvPackage is the pinned semantic-conventions package (ADR 0006).
const semconvPackage = "go.opentelemetry.io/otel/semconv/v1.39.0"

// attributePackage is the OpenTelemetry package whose functions and types take
// an attribute key.
const attributePackage = "go.opentelemetry.io/otel/attribute"

// catalogPath is the attribute catalog, read for the SDK-owned keys.
const catalogPath = "docs/semconv.md"

// sdkOwnedMarker is how a catalog row says the SDK defines the key itself.
const sdkOwnedMarker = "SDK-owned"

// lookupTimeout bounds the `go list` subprocess so a stalled module lookup
// fails with a clear message instead of consuming the whole CI job budget.
const lookupTimeout = 2 * time.Minute

// examplesDir holds the runnable examples at the repository root.
const examplesDir = "examples"

// skipDirs are directory names the go tool itself ignores, at any depth.
var skipDirs = map[string]bool{"testdata": true, "vendor": true}

// catalog is what the scan checks a key against.
type catalog struct {
	// keys are the attribute keys the pinned semconv package declares.
	keys map[string]bool
	// owned are the keys docs/semconv.md marks as SDK-owned. An entry ending
	// in ".*" covers every key under that prefix.
	owned map[string]bool
	// keyFuncs are the functions of the pinned attribute package that take an
	// attribute key, by name.
	keyFuncs map[string]keyArgs
}

// keyArgs says which arguments of an attribute-package function are keys.
type keyArgs struct {
	// from is the index of the first key argument.
	from int
	// rest reports that every argument after from is a key too, as for the
	// variadic filter constructors.
	rest bool
}

func main() {
	cat, err := loadCatalog(catalogPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "check_semconv_literals:", err)
		os.Exit(1)
	}
	findings, err := scan(".", cat)
	if err != nil {
		fmt.Fprintln(os.Stderr, "check_semconv_literals:", err)
		os.Exit(1)
	}
	if len(findings) > 0 {
		for _, f := range findings {
			fmt.Fprintln(os.Stderr, f)
		}
		fmt.Fprintf(os.Stderr,
			"%d finding(s). A key the pinned package declares is named through the %s constant (docs/semconv.md, Enforcement Rule 1). "+
				"A key in a semconv namespace that the pinned package does not declare is renamed, removed or misspelt — "+
				"resolve it with the verify-semconv-attributes skill — or is SDK-owned and needs a row in %s whose description begins with %q (Enforcement Rule 2).\n",
			len(findings), semconvPackage, catalogPath, sdkOwnedMarker)
		os.Exit(1)
	}
}

// loadCatalog reads the three sets the scan needs: the pinned keys, the
// attribute-package functions that take one, and the SDK-owned keys.
func loadCatalog(semconvDoc string) (catalog, error) {
	keys, err := semconvKeys()
	if err != nil {
		return catalog{}, err
	}
	keyFuncs, err := attributeKeyFuncs()
	if err != nil {
		return catalog{}, err
	}
	owned, err := sdkOwnedKeys(semconvDoc)
	if err != nil {
		return catalog{}, err
	}
	return catalog{keys: keys, owned: owned, keyFuncs: keyFuncs}, nil
}

// packageFiles parses the non-test Go files of the package at importPath, as
// the build resolves it.
func packageFiles(importPath string) ([]*ast.File, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "go", "list", "-f", "{{.Dir}}", importPath).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return nil, "", fmt.Errorf("locate %s: %w: %s", importPath, err, bytes.TrimSpace(exitErr.Stderr))
		}
		return nil, "", fmt.Errorf("locate %s: %w", importPath, err)
	}
	dir := strings.TrimSpace(string(out))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, "", err
	}
	var files []*ast.File
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			return nil, "", err
		}
		files = append(files, file)
	}
	return files, dir, nil
}

// semconvKeys returns every attribute key the pinned semconv package declares.
func semconvKeys() (map[string]bool, error) {
	files, dir, err := packageFiles(semconvPackage)
	if err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			if key, ok := attributeKeyCall(node); ok {
				keys[key] = true
			}
			return true
		})
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("found no attribute.Key declarations in %s", dir)
	}
	return keys, nil
}

// attributeKeyCall reports the key of an attribute.Key("...") call.
func attributeKeyCall(node ast.Node) (string, bool) {
	call, ok := node.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Key" {
		return "", false
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "attribute" {
		return "", false
	}
	return stringLiteral(call.Args[0])
}

// attributeKeyFuncs returns the exported functions of the pinned attribute
// package that take an attribute key, with the position of the key.
//
// Two shapes take one. The KeyValue constructors — String, Int, Bool and the
// rest — take it as a first parameter named k of type string. The filter
// constructors take a variadic list of Key. Reading them from the source
// rather than listing them means a constructor a later release adds is covered
// when the pin moves.
func attributeKeyFuncs() (map[string]keyArgs, error) {
	files, dir, err := packageFiles(attributePackage)
	if err != nil {
		return nil, err
	}
	funcs := map[string]keyArgs{}
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !fn.Name.IsExported() || fn.Type.Params == nil {
				continue
			}
			if args, ok := keyParams(fn.Type.Params.List); ok {
				funcs[fn.Name.Name] = args
			}
		}
	}
	// A partial read must fail rather than pass the scan with fewer positions
	// to look at: these two are the two shapes, and both have been in the
	// package since v1.0.
	for _, name := range []string{"String", "NewAllowKeysFilter"} {
		if _, ok := funcs[name]; !ok {
			return nil, fmt.Errorf("found no key parameter on %s.%s in %s", attributePackage, name, dir)
		}
	}
	return funcs, nil
}

// keyParams finds the key among a function's parameters.
func keyParams(params []*ast.Field) (keyArgs, bool) {
	index := 0
	for _, field := range params {
		names := max(len(field.Names), 1)
		switch t := field.Type.(type) {
		case *ast.Ident:
			if t.Name == "Key" {
				return keyArgs{from: index}, true
			}
			if index == 0 && t.Name == "string" && len(field.Names) > 0 && field.Names[0].Name == "k" {
				return keyArgs{from: 0}, true
			}
		case *ast.Ellipsis:
			if elt, ok := t.Elt.(*ast.Ident); ok && elt.Name == "Key" {
				return keyArgs{from: index, rest: true}, true
			}
		}
		index += names
	}
	return keyArgs{}, false
}

// backticked matches one `code span` of a Markdown table cell.
var backticked = regexp.MustCompile("`([^`]+)`")

// sdkOwnedKeys returns the keys docs/semconv.md marks as SDK-owned: every code
// span in the first cell of a table row whose last cell begins with
// "SDK-owned".
//
// Beginning with it is the convention, not containing it. "SDK-owned" also
// turns up mid-sentence in rows that are about something else — "emitted by
// the SDK-owned resty wrapper" describes the wrapper, and the key in that row
// is a semconv metric name.
func sdkOwnedKeys(path string) (map[string]bool, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the attribute catalog: %w", err)
	}
	owned := map[string]bool{}
	for line := range strings.SplitSeq(string(body), "\n") {
		cells := tableCells(line)
		if len(cells) < 2 {
			continue
		}
		// Emphasis around the marker does not change what the row says.
		description := strings.TrimLeft(cells[len(cells)-1], "*_")
		if !strings.HasPrefix(description, sdkOwnedMarker) {
			continue
		}
		for _, match := range backticked.FindAllStringSubmatch(cells[0], -1) {
			owned[match[1]] = true
		}
	}
	// The tree holds SDK-owned keys, so an empty set means the convention was
	// not found — a reworded table, a moved file — and every such key would be
	// reported as unknown, or none would if the scan trusted an empty list.
	if len(owned) == 0 {
		return nil, fmt.Errorf("%s marks no key as %s: expected table rows whose last cell begins with it", path, sdkOwnedMarker)
	}
	return owned, nil
}

// tableCells returns the trimmed cells of a Markdown table row, or nil for a
// line that is not one.
func tableCells(line string) []string {
	line = strings.TrimSpace(line)
	if len(line) < 2 || !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
		return nil
	}
	// An escaped pipe is cell content, not a separator.
	const escapedPipe = "\x00"
	line = strings.ReplaceAll(line[1:len(line)-1], `\|`, escapedPipe)
	cells := strings.Split(line, "|")
	for i, cell := range cells {
		cells[i] = strings.TrimSpace(strings.ReplaceAll(cell, escapedPipe, "|"))
	}
	return cells
}

// isOwned reports whether key is SDK-owned, by name or under an owned prefix.
func (c catalog) isOwned(key string) bool {
	if c.owned[key] {
		return true
	}
	for prefix := key; ; {
		i := strings.LastIndex(prefix, ".")
		if i < 0 {
			return false
		}
		prefix = prefix[:i]
		if c.owned[prefix+".*"] {
			return true
		}
	}
}

// unknownNamespace returns the semconv namespace key sits in when the pinned
// package uses that namespace and declares no such key, and "" otherwise.
//
// The namespace is the first dot-separated segment, taken from the pinned keys
// themselves: "db" from "db.system.name". A key with no dot has no namespace.
func (c catalog) unknownNamespace(key string) string {
	namespace, _, ok := strings.Cut(key, ".")
	if !ok || c.keys[key] || c.isOwned(key) {
		return ""
	}
	for pinned := range c.keys {
		if strings.HasPrefix(pinned, namespace+".") {
			return namespace
		}
	}
	return ""
}

// scan returns a finding for each string literal under root, in SDK code, that
// equals a pinned key, and for each key in an attribute-key position that sits
// in a pinned namespace without being a pinned or an SDK-owned key.
func scan(root string, cat catalog) ([]string, error) {
	// Files are grouped by directory so a key named through a constant can be
	// resolved against the declarations of its own package.
	dirs := map[string][]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || skipDirs[name] || path == filepath.Join(root, examplesDir)) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			dir := filepath.Dir(path)
			dirs[dir] = append(dirs[dir], path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	var findings []string
	for _, dir := range slices.Sorted(func(yield func(string) bool) {
		for dir := range dirs {
			if !yield(dir) {
				return
			}
		}
	}) {
		found, err := scanDir(dirs[dir], cat)
		if err != nil {
			return nil, err
		}
		findings = append(findings, found...)
	}
	return findings, nil
}

// scanDir checks the files of one directory.
func scanDir(paths []string, cat catalog) ([]string, error) {
	slices.Sort(paths)
	fset := token.NewFileSet()
	var files []*ast.File
	for _, path := range paths {
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		if !ast.IsGenerated(file) {
			files = append(files, file)
		}
	}
	consts := stringDecls(files)

	var findings []string
	seen := map[string]bool{}
	report := func(pos token.Pos, format string, args ...any) {
		finding := fmt.Sprintf("%s: ", fset.Position(pos)) + fmt.Sprintf(format, args...)
		if !seen[finding] {
			seen[finding] = true
			findings = append(findings, finding)
		}
	}
	for _, file := range files {
		// Rule 1: any literal that equals a pinned key.
		ast.Inspect(file, func(node ast.Node) bool {
			if _, ok := node.(*ast.ImportSpec); ok {
				return false
			}
			lit, ok := node.(*ast.BasicLit)
			if !ok {
				return true
			}
			if value, ok := stringLiteral(lit); ok && cat.keys[value] {
				report(lit.Pos(), "%q is a semconv attribute key", value)
			}
			return true
		})
		// Rule 2: a key in a key position.
		for _, expr := range keyPositions(file, cat.keyFuncs) {
			value, ok := stringValue(expr, consts, map[string]bool{})
			if !ok {
				continue
			}
			if cat.keys[value] {
				// A literal, and a constant declared with one, are already
				// reported above where the literal stands. A key put together
				// from parts is not a literal anywhere, so say it here.
				if _, assembled := ast.Unparen(expr).(*ast.BinaryExpr); assembled {
					report(expr.Pos(), "%q is a semconv attribute key, assembled here from parts", value)
				}
				continue
			}
			if namespace := cat.unknownNamespace(value); namespace != "" {
				report(expr.Pos(), "%q is not an attribute key of %s, whose %q namespace it is in", value, semconvPackage, namespace)
			}
		}
	}
	return findings, nil
}

// stringDecls returns the string constants and variables the files declare, by
// name, as the expression each is declared with.
//
// A name bound more than once anywhere in the directory is left out: declared
// twice, or also used as a parameter, a := variable or a range variable. Which
// binding a use refers to is a scoping question this scan does not answer, and
// guessing could report a key the code never uses — a package constant "name"
// holding a log message, say, against a function whose parameter is called
// name.
func stringDecls(files []*ast.File) map[string]ast.Expr {
	decls := map[string]ast.Expr{}
	ambiguous := map[string]bool{}
	rebound := func(names ...ast.Expr) {
		for _, name := range names {
			if ident, ok := name.(*ast.Ident); ok {
				ambiguous[ident.Name] = true
			}
		}
	}
	fields := func(list *ast.FieldList) {
		if list == nil {
			return
		}
		for _, field := range list.List {
			for _, name := range field.Names {
				ambiguous[name.Name] = true
			}
		}
	}
	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.ValueSpec:
				for i, name := range n.Names {
					if name.Name == "_" {
						continue
					}
					if _, dup := decls[name.Name]; dup || i >= len(n.Values) {
						ambiguous[name.Name] = true
						continue
					}
					decls[name.Name] = n.Values[i]
				}
			case *ast.FuncDecl:
				fields(n.Recv)
			case *ast.FuncType:
				fields(n.Params)
				fields(n.Results)
			case *ast.AssignStmt:
				if n.Tok == token.DEFINE {
					rebound(n.Lhs...)
				}
			case *ast.RangeStmt:
				if n.Tok == token.DEFINE {
					rebound(n.Key, n.Value)
				}
			}
			return true
		})
	}
	for name := range ambiguous {
		delete(decls, name)
	}
	return decls
}

// stringValue returns the string an expression in a key position denotes, when
// that can be told without running the program: a literal, a concatenation, a
// string(...) conversion of either, or the name of a declaration in decls that
// is one of those.
//
// An attribute.Key(...) conversion is not followed. It is a key position of its
// own and is checked where it stands, and following every call named Key would
// also follow ones that are not conversions at all.
func stringValue(expr ast.Expr, decls map[string]ast.Expr, visiting map[string]bool) (string, bool) {
	switch e := ast.Unparen(expr).(type) {
	case *ast.BasicLit:
		return stringLiteral(e)
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		left, ok := stringValue(e.X, decls, visiting)
		if !ok {
			return "", false
		}
		right, ok := stringValue(e.Y, decls, visiting)
		return left + right, ok
	case *ast.Ident:
		decl, ok := decls[e.Name]
		if !ok || visiting[e.Name] {
			return "", false
		}
		visiting[e.Name] = true
		defer delete(visiting, e.Name)
		return stringValue(decl, decls, visiting)
	case *ast.CallExpr:
		// string(k) converts without changing the text. The name is not
		// resolved either: a directory that declares its own "string" is not
		// one this scan needs to understand.
		if fun, ok := e.Fun.(*ast.Ident); !ok || fun.Name != "string" || len(e.Args) != 1 {
			return "", false
		}
		return stringValue(e.Args[0], decls, visiting)
	}
	return "", false
}

// keyPositions returns the expressions in file that stand where an attribute
// key goes. See the package comment for the list.
func keyPositions(file *ast.File, keyFuncs map[string]keyArgs) []ast.Expr {
	pkg := importName(file, attributePackage)
	if pkg == "" {
		return nil
	}
	// isAttr reports whether expr is the attribute package's name, e.g. Key.
	isAttr := func(expr ast.Expr, name string) bool {
		sel, ok := expr.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != name {
			return false
		}
		x, ok := sel.X.(*ast.Ident)
		return ok && x.Name == pkg
	}

	var positions []ast.Expr
	// keyValue adds the key of one attribute.KeyValue literal: the Key field,
	// or the first element when the fields are not named.
	keyValue := func(lit *ast.CompositeLit) {
		for i, elt := range lit.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				if field, ok := kv.Key.(*ast.Ident); ok && field.Name == "Key" {
					positions = append(positions, kv.Value)
				}
			} else if i == 0 {
				positions = append(positions, elt)
			}
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); !ok || x.Name != pkg {
				return true
			}
			if sel.Sel.Name == "Key" && len(n.Args) == 1 {
				positions = append(positions, n.Args[0])
				return true
			}
			args, ok := keyFuncs[sel.Sel.Name]
			if !ok || args.from >= len(n.Args) {
				return true
			}
			if args.rest {
				positions = append(positions, n.Args[args.from:]...)
			} else {
				positions = append(positions, n.Args[args.from])
			}
		case *ast.ValueSpec:
			if n.Type != nil && isAttr(n.Type, "Key") {
				positions = append(positions, n.Values...)
			}
		case *ast.CompositeLit:
			switch t := n.Type.(type) {
			case *ast.SelectorExpr:
				if isAttr(t, "KeyValue") {
					keyValue(n)
				}
			case *ast.ArrayType:
				for _, elt := range n.Elts {
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						elt = kv.Value
					}
					switch {
					case isAttr(t.Elt, "Key"):
						positions = append(positions, elt)
					case isAttr(t.Elt, "KeyValue"):
						// []attribute.KeyValue{{Key: k}}: the element type is
						// elided, so the literal is not seen as a KeyValue on
						// its own.
						if inner, ok := elt.(*ast.CompositeLit); ok && inner.Type == nil {
							keyValue(inner)
						}
					}
				}
			case *ast.MapType:
				for _, elt := range n.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if isAttr(t.Key, "Key") {
						positions = append(positions, kv.Key)
					}
					if isAttr(t.Value, "Key") {
						positions = append(positions, kv.Value)
					}
				}
			}
		}
		return true
	})
	return positions
}

// importName returns the name file refers to the package at importPath by, or
// "" when the file does not import it under a usable name.
func importName(file *ast.File, importPath string) string {
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || path != importPath {
			continue
		}
		if spec.Name == nil {
			return importPath[strings.LastIndex(importPath, "/")+1:]
		}
		if spec.Name.Name == "_" || spec.Name.Name == "." {
			return ""
		}
		return spec.Name.Name
	}
	return ""
}

// stringLiteral returns the value of a string literal expression.
func stringLiteral(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(lit.Value)
	return value, err == nil
}
