//go:build ignore

package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var integrationDirs = []string{"http", "nats", "mongo", "gin", "resty", "redis", "minio"}

const otelModulePath = "go.opentelemetry.io/otel"

var setterNames = map[string]struct{}{
	"SetTracerProvider":    {},
	"SetTextMapPropagator": {},
	"SetMeterProvider":     {},
	"SetLoggerProvider":    {},
}

func main() {
	var errs []error
	errs = append(errs, checkGoModIntegrations()...)
	errs = append(errs, checkTierAnnotations()...)
	errs = append(errs, checkNoGlobalSetters()...)
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
			strings.HasPrefix(module, "github.com/Marz32onE/instrumentation-go/") ||
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

func checkNoGlobalSetters() []error {
	var errs []error
	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			errs = append(errs, fmt.Errorf("walk %s: %w", path, err))
			return nil
		}
		if d.IsDir() && shouldSkipDir(d.Name()) {
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
		info := typeInfoForFile(fset, file)
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if ok {
				if ident, ok := call.Fun.(*ast.Ident); ok {
					if _, forbidden := setterNames[ident.Name]; forbidden {
						if isOTelObject(info.Uses[ident]) {
							errs = append(errs, fmt.Errorf("%s: forbidden otel.%s call", fset.Position(ident.Pos()), ident.Name))
						}
					}
				}
			}

			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if !isOTelPackageRoot(info.Uses[ident]) {
				return true
			}
			if _, forbidden := setterNames[sel.Sel.Name]; forbidden {
				errs = append(errs, fmt.Errorf("%s: forbidden otel.%s call", fset.Position(sel.Pos()), sel.Sel.Name))
			}
			return true
		})
		return nil
	})
	if err != nil {
		errs = append(errs, err)
	}
	return errs
}

func typeInfoForFile(fset *token.FileSet, file *ast.File) *types.Info {
	info := &types.Info{
		Uses: make(map[*ast.Ident]types.Object),
	}
	conf := types.Config{
		Importer: policyImporter{},
		Error:    func(error) {},
	}
	_, _ = conf.Check(file.Name.Name, fset, []*ast.File{file}, info)
	return info
}

func isOTelPackageRoot(obj types.Object) bool {
	pkgName, ok := obj.(*types.PkgName)
	if !ok || pkgName.Imported() == nil {
		return false
	}
	return pkgName.Imported().Path() == otelModulePath
}

func isOTelObject(obj types.Object) bool {
	if obj == nil {
		return false
	}
	if isOTelPackageRoot(obj) {
		return true
	}
	pkg := obj.Pkg()
	return pkg != nil && pkg.Path() == otelModulePath
}

type policyImporter struct{}

func (policyImporter) Import(path string) (*types.Package, error) {
	pkg := types.NewPackage(path, packageName(path))
	if path == otelModulePath {
		empty := types.NewTuple()
		sig := types.NewSignatureType(nil, nil, nil, empty, empty, false)
		for name := range setterNames {
			pkg.Scope().Insert(types.NewFunc(token.NoPos, pkg, name, sig))
		}
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
