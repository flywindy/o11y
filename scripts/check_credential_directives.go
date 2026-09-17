//go:build ignore

// Command check_credential_directives enforces that a credential suppression
// names every scanner that would otherwise report the line.
//
// Two scanners see these fixtures and neither silences the other:
//
//	#nosec G101                     gosec's own rule, run by `make sast-gosec-cred`
//	nosemgrep: gosec.G101-1         what an external scan reports, run nowhere here
//	nosemgrep: hardcoded-credential-literal   this repo's rule, run by `make sast-semgrep`
//
// nosemgrep matches a rule id by exact suffix, so naming one never silences
// another, and gosec's directive is a separate mechanism again. A fixture that
// names only the scanners running in CI therefore looks annotated, passes every
// gate here, and is reported the first time the tree is scanned elsewhere —
// which is what happened to internal/log/logr_test.go.
//
// Since gosec.G101-1 is not in the public semgrep registry (see the note on
// SEMGREP_FLAGS), no gate can run that rule. Enforcing that it is named
// wherever a sibling scanner is known to fire is what this repo can do instead.
//
// Four signals say a scanner fires on a line, and each requires the external id
// alongside it:
//
//	nosemgrep: hardcoded-credential-literal   this repo's rule fires here
//	#nosec covering G101                      gosec fires here
//	ruleid: hardcoded-credential-literal      a .semgrep/ fixture asserts it fires
//	a credential-shaped binding in .semgrep/  a planted credential, asserted either way
//
// The last two matter most for the directory the other gates all exclude.
// .semgrep/ holds deliberate violations, so GOSEC_FLAGS, SEMGREP_FLAGS and
// .semgrepignore skip it — but those are repo-local, and the fixture file says
// so itself: an external scan reads the files directly and reports them
// whatever the ignore list holds, which is what happened the first time this
// tree was scanned elsewhere. The directory the convention matters most in was
// the one nothing checked.
//
// A fixture's negatives need the directive as much as its positives: a value
// the repo's rule deliberately does not match (a composite-literal field, say)
// is still a planted credential on disk. Rather than a second copy of the rule
// that can drift from it, the check parses the fixture and reads the rule's own
// $NAME regex out of the sibling .yml, so both key on the same identifiers.
//
// Comments come from go/parser rather than a line scan, because gosec honors a
// block-form directive too. Verified against gosec v2.26.1 over a
// password-in-URL fixture: `/* #nosec G101 */` above the declaration, the same
// trailing it, and a multi-line block with the tag on its own line all suppress
// the finding. A scan that recognizes only `//` sees none of them, and the
// identifiers these fixtures use are not credential-shaped, so nothing else
// here would fire either.
package main

import (
	"bufio"
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
	"strings"
)

// registryID is the rule id an external scan reports for a hardcoded credential.
const registryID = "gosec.G101-1"

// repoRuleID is this repository's own rule, defined in .semgrep/.
const repoRuleID = "hardcoded-credential-literal"

// selfName is this command's own file, excluded from the walk below.
const selfName = "check_credential_directives.go"

// fixtureDir holds the semgrep rules and their fixtures.
const fixtureDir = ".semgrep"

// gosecTags are the build tags GOSEC_CRED_FLAGS passes to gosec.
var gosecTags = []string{"integration"}

// gosecExempt lists the files the toolchain excludes from `./...`, with the
// reason each is accepted anyway. Listing paths rather than a rule is
// deliberate: `//go:build ignore` would otherwise be a way to put a file
// outside the credential gate without anyone noticing.
var gosecExempt = map[string]string{
	// Standalone `go run` programs, never part of a build, and being two
	// package main files in one directory they cannot be handed to gosec
	// together. Not uncovered: the repo's semgrep rule and this checker both
	// read files rather than packages, so only gosec's neutral-identifier URL
	// class is missing for them.
	"scripts/check_credential_directives.go": "//go:build ignore standalone program",
	"scripts/check_integrations.go":          "//go:build ignore standalone program",
	// A dot-directory, invisible to the go tool by definition. checkFixture
	// parses it directly, which is the whole reason that check exists.
	".semgrep/hardcoded-credentials.go": "semgrep rule fixture, covered by checkFixture",
}

// notFirstParty are directories whose contents this repo does not author, and
// which `go list ./...` does not report either -- wildcard patterns skip a
// vendor path element by definition (`go help packages`), and the rest hold
// dependencies or build output. Walking them would report every file in them
// as outside the census, failing the gate on code nobody here wrote.
//
// The list mirrors .semgrepignore, with one deliberate difference: .semgrep/ is
// excluded there and walked here, because its fixtures are the files this gate
// exists to check.
var notFirstParty = []string{
	"vendor", "node_modules", "build", "dist",
	".venv", "venv", ".env",
	".git", ".svn", ".hg",
}

// urlCredential matches a URL literal carrying a password in its userinfo,
// the one credential shape whose identifier says nothing about it.
var urlCredential = regexp.MustCompile(`(?i)[a-z][a-z0-9+.\-]*://[^/\s:@"` + "`" + `]+:[^/\s@"` + "`" + `]+@`)

// nosecTag is gosec's suppression directive.
const nosecTag = "#nosec"

// nosecRuleID matches one rule id in a gosec directive's list (G101, G304).
var nosecRuleID = regexp.MustCompile(`^[A-Z]+[0-9]+$`)

type violation struct {
	file string
	line int
	text string
	why  string
}

func main() {
	violations, err := scan(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "check_credential_directives: %v\n", err)
		os.Exit(1)
	}
	if len(violations) > 0 {
		for _, v := range violations {
			fmt.Fprintf(os.Stderr, "%s:%d: %s\n    %s\n", v.file, v.line, v.why, strings.TrimSpace(v.text))
		}
		fmt.Fprintf(os.Stderr, "\n%d credential suppression(s) missing %s\n", len(violations), registryID)
		os.Exit(1)
	}
	fmt.Println("==> credential suppression directives: OK")
}

// scan walks root for Go files and returns every directive violation found.
//
// .semgrep/ is walked like anything else, and gets the extra fixture check:
// every other gate excludes that directory, so it is the one place where a
// missing directive is invisible until an external scan reads the file.
func scan(root string) ([]violation, error) {
	self := filepath.Join("scripts", selfName)
	reachable, err := reachableFiles()
	if err != nil {
		return nil, err
	}
	var found []violation
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if slices.Contains(notFirstParty, d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		lines, err := readLines(path)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		src := newSource(fset, parsed, lines)

		// This file necessarily contains the directive patterns it searches
		// for, in its own doc comment and in the text of the messages it
		// prints, so those invariants would report itself. That is a reason to
		// skip the directives, not the values: the URL check below keys on
		// what a literal holds, so it applies here like anywhere gosec cannot
		// read. Matched by path, not basename -- a file of the same name in
		// another package is ordinary source and is checked like any other.
		isSelf := filepath.Clean(path) == self
		if !isSelf {
			found = append(found, check(path, src)...)
			found = append(found, checkReachable(path, reachable, lines)...)
		}
		// Files gosec's scan never reads, so its G101 cannot cover the one
		// credential shape the repo rule is blind to.
		if _, exempt := gosecExempt[filepath.ToSlash(filepath.Clean(path))]; exempt || isSelf {
			found = append(found, checkURLLiterals(path, src, parsed)...)
		}

		if filepath.Base(filepath.Dir(path)) == fixtureDir {
			fixtureFound, err := checkFixture(path, src, parsed)
			if err != nil {
				return err
			}
			found = append(found, fixtureFound...)
		}
		return nil
	})
	return found, err
}

// check applies the three directive invariants to one file.
//
// They are deliberately one-directional. Naming the repo's rule, suppressing
// gosec's, or asserting the repo's rule fires all mean the line is a
// credential-shaped value that an external scan reports too, so the registry
// id has to be named as well. The converse is not a violation: the URL
// fixtures trip only the external rule, because their identifiers are not
// credential-shaped, and name only that id.
func check(file string, src *source) []violation {
	var found []violation
	for _, c := range src.comments {
		ids := c.nosemgrepIDs()
		if names(ids, repoRuleID) && !names(ids, registryID) {
			found = append(found, src.violation(file, c.startLine,
				"nosemgrep directive names "+repoRuleID+" but not "+registryID))
		}
		if c.nosecCoversG101() && !src.namedInSlot(src.statement(c)) {
			found = append(found, src.violation(file, c.startLine,
				"gosec directive suppresses G101 with no nosemgrep: "+registryID+
					" on the line it covers"))
		}
	}
	return found
}

// reachableFiles returns the absolute paths of the Go files the toolchain
// includes under the credential gate's tags -- which is exactly what
// `gosec ./...` reads.
//
// The go tool is asked rather than modelled. A file leaves `./...` through an
// unsatisfied //go:build expression, an implicit constraint from a _GOOS or
// _GOARCH filename, a testdata/ or dot-directory, or a rule nobody here
// remembered; reimplementing those is how a coverage check ends up confidently
// wrong, which an earlier version of this one was -- it read //go:build
// identifiers as coverage and so accepted `//go:build !integration`, the one
// expression the gate's own -tags excludes.
func reachableFiles() (map[string]bool, error) {
	// Every field holding a .go file the toolchain includes. GoFiles is
	// documented as excluding CgoFiles, so omitting the latter would report a
	// cgo source as unreachable and fail the gate on valid code.
	const tmpl = `{{$d := .Dir}}` +
		`{{range .GoFiles}}{{$d}}/{{.}}
{{end}}{{range .CgoFiles}}{{$d}}/{{.}}
{{end}}{{range .TestGoFiles}}{{$d}}/{{.}}
{{end}}{{range .XTestGoFiles}}{{$d}}/{{.}}
{{end}}`
	// #nosec G204 -- the tag list is a constant in this file, not input
	cmd := exec.Command("go", "list", "-tags", strings.Join(gosecTags, ","), "-e", "-f", tmpl, "./...")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %w", err)
	}
	files := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files[line] = true
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("go list reported no Go files; the census would pass vacuously")
	}
	return files, nil
}

// checkReachable reports a Go file that gosec's scan cannot read, so the
// credential gate never sees it.
//
// The repo rule will not fire on a neutral identifier there either, and an
// unannotated credential leaves this checker nothing to inspect -- so such a
// file is invisible to every local gate while an external scan still reads it.
// Reporting it turns that into a decision someone has to make: pass the tag,
// move the file, or record it in gosecExempt with a reason.
func checkReachable(file string, reachable map[string]bool, lines []string) []violation {
	if _, ok := gosecExempt[filepath.ToSlash(filepath.Clean(file))]; ok {
		return nil
	}
	abs, err := filepath.Abs(file)
	if err != nil || reachable[filepath.ToSlash(abs)] {
		return nil
	}
	text := ""
	if len(lines) > 0 {
		text = lines[0]
	}
	return []violation{{file, 1, text,
		"the go tool excludes this file from ./..., so gosec never reads it — " +
			"check its build constraints and filename, or record it in gosecExempt"}}
}

// checkURLLiterals requires the registry id on every URL literal carrying a
// password, wherever it sits.
//
// This is the one credential shape the repo rule's $NAME regex cannot see,
// because the identifier holding it says nothing -- `endpoint`, `target`,
// `addr`. Everywhere gosec reads, its G101 covers the shape and this check
// would only duplicate it; it runs over the files gosec does not read, where
// otherwise nothing would.
func checkURLLiterals(file string, src *source, parsed *ast.File) []violation {
	var found []violation
	reported := map[int]bool{}
	ast.Inspect(parsed, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING || !urlCredential.MatchString(lit.Value) {
			return true
		}
		line := src.fset.Position(lit.Pos()).Line
		if src.namedInSlot(line) || reported[line] {
			return true
		}
		reported[line] = true
		found = append(found, src.violation(file, line,
			"password in a URL does not name "+registryID))
		return true
	})
	return found
}

// checkFixture requires the registry id on every credential-shaped binding in
// a rule fixture, positive or negative.
//
// The rule's own $NAME regex decides what counts, read from the sibling .yml so
// there is one definition rather than two that drift. The value must be a string
// literal: `password := lookup("GRAPH_PROXY_PASSWORD")` names an env var, it does
// not hold a credential.
func checkFixture(file string, src *source, parsed *ast.File) ([]violation, error) {
	ruleFile := strings.TrimSuffix(file, ".go") + ".yml"
	if _, err := os.Stat(ruleFile); err != nil {
		return nil, nil // a .go without a sibling rule is not a fixture
	}
	// Only the credential rule's own fixture. Another rule's fixture is not
	// credential data, and demanding a $NAME regex of every rule would fail
	// this gate the moment someone adds one written with a direct pattern.
	nameRe, err := credentialNameRegex(ruleFile, repoRuleID)
	if err != nil {
		return nil, err
	}
	if nameRe == nil {
		return nil, nil
	}

	var found []violation
	report := func(pos token.Pos, name string) {
		line := src.fset.Position(pos).Line
		if src.namedInSlot(line) {
			return
		}
		found = append(found, src.violation(file, line,
			"planted credential "+name+" does not name "+registryID))
	}

	ast.Inspect(parsed, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.ValueSpec:
			for i, name := range v.Names {
				if i < len(v.Values) && nameRe.MatchString(name.Name) && isStringLiteral(v.Values[i]) {
					report(v.Values[i].Pos(), name.Name)
				}
			}
		case *ast.AssignStmt:
			for i, lhs := range v.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || i >= len(v.Rhs) {
					continue
				}
				if nameRe.MatchString(id.Name) && isStringLiteral(v.Rhs[i]) {
					report(v.Rhs[i].Pos(), id.Name)
				}
			}
		case *ast.KeyValueExpr:
			// A composite-literal field, where the key names the credential and
			// the value is the literal: a ProxyPassword or a ClientSecret.
			if id, ok := v.Key.(*ast.Ident); ok &&
				nameRe.MatchString(id.Name) && isStringLiteral(v.Value) {
				report(v.Value.Pos(), id.Name)
			}
		}
		return true
	})
	return found, nil
}

// credentialNameRegex reads the $NAME metavariable-regex belonging to the named
// rule, or (nil, nil) when the file declares no such rule.
//
// Parsed by hand rather than with a YAML library so this gate adds no
// dependency; an extraction failure inside the rule is an error rather than a
// skip, because a silently empty pattern is the no-op gate this whole target
// exists to prevent.
//
// Two bounds keep the answer the right rule's. The search runs inside the list
// item whose `id` matches, so a sibling rule cannot lend its regex; and within
// that, inside the block $NAME's own constraint opens, so $VAL cannot -- its
// regex matches string literals rather than identifiers, which matches no
// identifier at all and would check nothing while reporting OK.
func credentialNameRegex(ruleFile, id string) (*regexp.Regexp, error) {
	lines, err := readLines(ruleFile)
	if err != nil {
		return nil, err
	}
	lo, hi, ok, err := ruleBlock(lines, id)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", ruleFile, err)
	}
	if !ok {
		return nil, nil
	}
	for i := lo; i < hi; i++ {
		key, value, ok := yamlEntry(strings.TrimSpace(lines[i]))
		if !ok || key != "metavariable" || yamlScalar(value) != "$NAME" {
			continue
		}
		base := indentOf(lines[i])
		for j := i + 1; j < hi; j++ {
			trimmed := strings.TrimSpace(lines[j])
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			nextKey, nextValue, ok := yamlEntry(trimmed)
			if indentOf(lines[j]) < base || (ok && nextKey == "metavariable") {
				break // left $NAME's constraint block
			}
			if !ok || nextKey != "regex" {
				continue
			}
			if isBlockScalar(nextValue) {
				return nil, fmt.Errorf("the $NAME regex in %s is a YAML block scalar, "+
					"which this reader does not support; write it inline", ruleFile)
			}
			re, err := regexp.Compile(yamlScalar(nextValue))
			if err != nil {
				return nil, fmt.Errorf("compile $NAME regex from %s: %w", ruleFile, err)
			}
			return re, nil
		}
		return nil, fmt.Errorf("no regex in the $NAME constraint block of %s", ruleFile)
	}
	return nil, fmt.Errorf("no $NAME metavariable-regex in rule %s of %s", id, ruleFile)
}

// ruleBlock returns the line range of the rules-list item declaring the given
// id. Items are recognized by the indent of the first one under `rules:`, so a
// nested list (patterns, pattern-either) does not read as a new rule.
//
// An id this reader cannot resolve is an error, not a miss. Reporting "no such
// rule" for one it simply could not read would skip the fixture check without
// saying so, which is the failure this gate exists to prevent.
func ruleBlock(lines []string, id string) (lo, hi int, ok bool, err error) {
	rulesAt := -1
	for i, line := range lines {
		if key, _, ok := yamlEntry(strings.TrimSpace(line)); ok && key == "rules" {
			rulesAt = i
			break
		}
	}
	if rulesAt < 0 {
		return 0, 0, false, nil
	}

	itemIndent, starts := -1, []int{}
	for i := rulesAt + 1; i < len(lines); i++ {
		if !strings.HasPrefix(strings.TrimSpace(lines[i]), "- ") {
			continue
		}
		switch indent := indentOf(lines[i]); {
		case itemIndent < 0:
			itemIndent = indent
			starts = append(starts, i)
		case indent == itemIndent:
			starts = append(starts, i)
		}
	}

	for n, from := range starts {
		to := len(lines)
		if n+1 < len(starts) {
			to = starts[n+1]
		}
		for i := from; i < to; i++ {
			key, value, ok := yamlEntry(strings.TrimPrefix(strings.TrimSpace(lines[i]), "- "))
			if !ok || key != "id" {
				continue
			}
			if isBlockScalar(value) {
				return 0, 0, false, fmt.Errorf("rule id on line %d is a YAML block scalar, "+
					"which this reader does not support; write it inline", i+1)
			}
			if yamlScalar(value) == id {
				return from, to, true, nil
			}
		}
	}
	return 0, 0, false, nil
}

// yamlEntry splits a mapping entry into its key and value. The key is trimmed
// because `id : x` is the same YAML as `id: x`, and a check that accepts only
// one spelling of it stops running without saying so.
func yamlEntry(line string) (key, value string, ok bool) {
	key, value, ok = strings.Cut(line, ":")
	return strings.TrimSpace(key), value, ok
}

// yamlScalar normalizes a YAML scalar value: a quoted one yields its contents,
// a plain one is cut at an inline comment.
func yamlScalar(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if quote := value[0]; quote == '"' || quote == '\'' {
		if end := strings.IndexByte(value[1:], quote); end >= 0 {
			return value[1 : 1+end]
		}
		return strings.Trim(value, string(quote))
	}
	if cut := strings.Index(value, " #"); cut >= 0 {
		value = value[:cut]
	}
	return strings.TrimSpace(value)
}

// isBlockScalar reports whether a value is a block-scalar header (`|` or `>`,
// with optional chomping and indent indicators) rather than an inline scalar.
//
// The contents of such a scalar live on the following lines, so a reader that
// takes the header gets the marker itself -- and `>-` compiles as a perfectly
// valid regex that matches those two characters and no identifier at all. That
// is the silent no-op this gate exists to prevent, so it is an error here
// rather than something to half-support.
func isBlockScalar(value string) bool {
	value = strings.TrimSpace(value)
	if cut := strings.Index(value, " #"); cut >= 0 {
		value = strings.TrimSpace(value[:cut])
	}
	if value == "" || (value[0] != '|' && value[0] != '>') {
		return false
	}
	for _, r := range value[1:] {
		if !strings.ContainsRune("+-0123456789", r) {
			return false
		}
	}
	return true
}

// indentOf returns a line's leading-space count, YAML's block structure.
func indentOf(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}

// isStringLiteral reports whether e is a non-empty string literal, or a
// concatenation of them ("sup3r-" + "secret" is as hard-coded as one literal).
//
// Empty is excluded to match the rule's own $VAL regex, which requires at least
// one character in every component. `password := ""` is an empty default, not a
// planted credential, and demanding a suppression for it would be the gate
// failing on ordinary code.
func isStringLiteral(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.BasicLit:
		return v.Kind == token.STRING && len(v.Value) > 2
	case *ast.BinaryExpr:
		return v.Op == token.ADD && isStringLiteral(v.X) && isStringLiteral(v.Y)
	}
	return false
}

// comment is one parsed Go comment, line or block, with the directive-bearing
// text of each of its lines.
type comment struct {
	startLine int
	endLine   int
	trailing  bool // code precedes it on its first line
	texts     []string
}

// source is a parsed file: its lines, its comments, and where each one sits.
type source struct {
	fset     *token.FileSet
	lines    []string
	comments []*comment
	covered  map[int]bool // lines holding nothing but a comment
}

func newSource(fset *token.FileSet, parsed *ast.File, lines []string) *source {
	s := &source{fset: fset, lines: lines, covered: map[int]bool{}}
	for _, group := range parsed.Comments {
		for _, c := range group.List {
			pos := fset.Position(c.Pos())
			before := ""
			if pos.Line-1 < len(lines) {
				line := lines[pos.Line-1]
				before = line[:min(pos.Column-1, len(line))]
			}
			parsed := &comment{
				startLine: pos.Line,
				endLine:   fset.Position(c.End()).Line,
				trailing:  strings.TrimSpace(before) != "",
				texts:     commentTexts(c.Text),
			}
			s.comments = append(s.comments, parsed)
			for l := parsed.startLine; l <= parsed.endLine; l++ {
				if l == parsed.startLine && parsed.trailing {
					continue
				}
				s.covered[l] = true
			}
		}
	}
	return s
}

// commentTexts returns a comment's text, one entry per line, with the markers
// and indentation stripped. A block comment can carry its directive on any of
// its lines -- gosec honors the multi-line form.
func commentTexts(raw string) []string {
	if after, ok := strings.CutPrefix(raw, "//"); ok {
		return []string{strings.TrimSpace(after)}
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(raw, "/*"), "*/")
	var texts []string
	for _, line := range strings.Split(inner, "\n") {
		texts = append(texts, strings.TrimSpace(line))
	}
	return texts
}

// statement returns the line the comment's directive governs: for a trailing
// comment its own line, otherwise the next line that is neither blank nor
// comment (gosec honors a directive across a blank line). It returns -1 when a
// comment at the end of a file governs nothing.
func (s *source) statement(c *comment) int {
	if c.trailing {
		return c.startLine
	}
	for l := c.endLine + 1; l <= len(s.lines); l++ {
		if s.covered[l] || strings.TrimSpace(s.lines[l-1]) == "" {
			continue
		}
		return l
	}
	return -1
}

// namedInSlot reports whether the registry id is named where semgrep would read
// it for the statement on the given line: in a comment trailing that line, or in
// one ending on the line directly above it. Binding the two directives to one
// statement is what stops a suppression borrowing its neighbour's -- semgrep
// honors a nosemgrep comment only on the finding's own line or the one above.
func (s *source) namedInSlot(stmt int) bool {
	if stmt < 1 {
		return false
	}
	for _, c := range s.comments {
		inSlot := (c.trailing && c.startLine == stmt) || c.endLine == stmt-1
		if inSlot && names(c.nosemgrepIDs(), registryID) {
			return true
		}
	}
	return false
}

func (s *source) violation(file string, line int, why string) violation {
	text := ""
	if line >= 1 && line <= len(s.lines) {
		text = s.lines[line-1]
	}
	return violation{file, line, text, why}
}

// nosecCoversG101 reports whether the comment is a gosec directive suppressing
// G101 -- by naming it, or by naming no rule at all, which suppresses every
// rule including this one.
//
// gosec honors the tag only at the start of a comment line. Verified against
// gosec v2.26.1 over a password-in-URL fixture: "#nosec", "#nosec -- reason"
// and "#nosec is not needed here" all suppress the finding, while
// "... so it needs no #nosec." and "see the note, #nosec G101" leave it
// reported. So prose that mentions the tag mid-sentence is not a directive,
// and a directive that names nothing is a blanket one.
func (c *comment) nosecCoversG101() bool {
	for _, text := range c.texts {
		rest, ok := strings.CutPrefix(text, nosecTag)
		if !ok {
			continue
		}
		if rest != "" && rest[0] != ' ' && rest[0] != '\t' && rest[0] != '-' {
			continue // "#nosecurity", not a directive
		}
		if ids := nosecRuleIDs(rest); len(ids) == 0 || slices.Contains(ids, "G101") {
			return true
		}
	}
	return false
}

// nosecRuleIDs returns the rule ids a gosec directive names. A reason may
// follow "--"; the list ends at the first token that is not a rule id.
func nosecRuleIDs(rest string) []string {
	if before, _, ok := strings.Cut(rest, "--"); ok {
		rest = before
	}
	var ids []string
	for _, field := range strings.FieldsFunc(rest, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	}) {
		if !nosecRuleID.MatchString(field) {
			break
		}
		ids = append(ids, field)
	}
	return ids
}

// ruleIDs returns the rule ids a semgrep test annotation names. The tag has to
// open the comment, as semgrep's test runner reads it -- prose mentioning it
// mid-sentence is documentation, not an assertion.
func (c *comment) ruleIDs() []string {
	for _, text := range c.texts {
		if rest, ok := strings.CutPrefix(text, "ruleid:"); ok {
			return splitIDs(rest)
		}
	}
	return nil
}

// nosemgrepIDs returns the rule ids a nosemgrep directive in the comment names,
// or nil when it carries none. A reason may follow "--"; ids precede it.
func (c *comment) nosemgrepIDs() []string {
	var ids []string
	for _, text := range c.texts {
		_, after, found := strings.Cut(text, "nosemgrep:")
		if !found {
			continue
		}
		ids = append(ids, splitIDs(after)...)
	}
	return ids
}

// splitIDs parses a comma-separated rule-id list, dropping any "--" reason.
func splitIDs(list string) []string {
	if before, _, ok := strings.Cut(list, "--"); ok {
		list = before
	}
	var ids []string
	for _, field := range strings.Split(list, ",") {
		id := strings.Trim(strings.TrimSpace(field), "`")
		// An id never contains whitespace, so anything after the first break is
		// surrounding prose rather than part of the id.
		if cut := strings.IndexAny(id, " \t`"); cut >= 0 {
			id = id[:cut]
		}
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// names reports whether ids contains want exactly.
//
// The ids are compared exactly rather than searched for as a substring: prose
// explaining the convention mentions the id without suppressing anything, and a
// substring match would also accept a different rule whose id merely starts
// with this one (gosec.G101-10).
func names(ids []string, want string) bool {
	return slices.Contains(ids, want)
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path) // #nosec G304 -- paths come from this repo's own tree walk
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var lines []string
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for s.Scan() {
		lines = append(lines, s.Text())
	}
	return lines, s.Err()
}
