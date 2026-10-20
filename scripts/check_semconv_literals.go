//go:build ignore

// Command check_semconv_literals enforces docs/semconv.md Enforcement Rule 1:
// SDK code names a semantic-convention attribute key through the constant in
// the pinned semconv package, never as a string literal.
//
// The key set is read from the pinned package's own source, every
// attribute.Key("...") it declares, so it follows the go.mod pin with nothing
// to regenerate. A string literal in SDK code that equals one of those keys is
// reported with the line it is on.
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
// A literal is the only form checked: a key assembled at run time, or one the
// pinned package does not declare, is left to review.
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
	"strconv"
	"strings"
	"time"
)

// semconvPackage is the pinned semantic-conventions package (ADR 0006).
const semconvPackage = "go.opentelemetry.io/otel/semconv/v1.39.0"

// lookupTimeout bounds the `go list` subprocess so a stalled module lookup
// fails with a clear message instead of consuming the whole CI job budget.
const lookupTimeout = 2 * time.Minute

// examplesDir holds the runnable examples at the repository root.
const examplesDir = "examples"

// skipDirs are directory names the go tool itself ignores, at any depth.
var skipDirs = map[string]bool{"testdata": true, "vendor": true}

func main() {
	keys, err := semconvKeys()
	if err != nil {
		fmt.Fprintln(os.Stderr, "check_semconv_literals:", err)
		os.Exit(1)
	}
	findings, err := scan(".", keys)
	if err != nil {
		fmt.Fprintln(os.Stderr, "check_semconv_literals:", err)
		os.Exit(1)
	}
	if len(findings) > 0 {
		for _, f := range findings {
			fmt.Fprintln(os.Stderr, f)
		}
		fmt.Fprintf(os.Stderr, "%d semconv attribute key(s) written as string literals; use the %s constant instead (docs/semconv.md, Enforcement Rule 1)\n", len(findings), semconvPackage)
		os.Exit(1)
	}
}

// semconvKeys returns every attribute key the pinned semconv package declares.
func semconvKeys() (map[string]bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "go", "list", "-f", "{{.Dir}}", semconvPackage).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return nil, fmt.Errorf("locate %s: %w: %s", semconvPackage, err, bytes.TrimSpace(exitErr.Stderr))
		}
		return nil, fmt.Errorf("locate %s: %w", semconvPackage, err)
	}
	dir := strings.TrimSpace(string(out))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			return nil, err
		}
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

// scan returns a finding for each string literal under root, in SDK code,
// that equals a key.
func scan(root string, keys map[string]bool) ([]string, error) {
	var findings []string
	fset := token.NewFileSet()
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
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		if ast.IsGenerated(file) {
			return nil
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if _, ok := node.(*ast.ImportSpec); ok {
				return false
			}
			lit, ok := node.(*ast.BasicLit)
			if !ok {
				return true
			}
			if value, ok := stringLiteral(lit); ok && keys[value] {
				findings = append(findings, fmt.Sprintf("%s: %q is a semconv attribute key", fset.Position(lit.Pos()), value))
			}
			return true
		})
		return nil
	})
	return findings, err
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
