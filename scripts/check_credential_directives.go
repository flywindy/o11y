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
// Three signals say a scanner fires on a line, and each requires the external
// id alongside it:
//
//	nosemgrep: hardcoded-credential-literal   this repo's rule fires here
//	#nosec G101                               gosec fires here
//	ruleid: hardcoded-credential-literal      a .semgrep/ fixture asserts it fires
//
// The third matters most for the directory the other gates all exclude.
// .semgrep/ holds deliberate violations, so GOSEC_FLAGS, SEMGREP_FLAGS and
// .semgrepignore skip it — but those are repo-local, and the fixture file says
// so itself: an external scan reads the files directly and reports them
// whatever the ignore list holds, which is what happened the first time this
// tree was scanned elsewhere. The directory the convention matters most in was
// the one nothing checked.
package main

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// registryID is the rule id an external scan reports for a hardcoded credential.
const registryID = "gosec.G101-1"

// repoRuleID is this repository's own rule, defined in .semgrep/.
const repoRuleID = "hardcoded-credential-literal"

// nosecG101 matches a gosec suppression naming G101. The rule id must follow
// the directive, so prose that merely mentions "#nosec" is not a directive.
var nosecG101 = regexp.MustCompile(`#nosec[ \t]+[A-Z0-9, \t]*G101`)

// ruleIDMarker is how a .semgrep/ fixture asserts the repo rule fires on the
// line below it.
const ruleIDMarker = "ruleid: " + repoRuleID

// selfName is this command's own file, excluded from the walk below.
const selfName = "check_credential_directives.go"

// pairWindow is how far from a #nosec directive its nosemgrep sibling may sit.
// The house style puts them adjacent in the comment block above a declaration;
// three lines allows a reason line between them.
const pairWindow = 3

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
// .semgrep/ is walked like anything else. Its fixtures name gosec.G101-1 alone
// by design, so the repo's own rule stays live for the rule's own assertions —
// and checking that they do name it is the point, since every other gate
// excludes that directory.
func scan(root string) ([]violation, error) {
	var found []violation
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		// This file necessarily contains the patterns it searches for, in its
		// own doc comment and in the text of the messages it prints.
		if d.Name() == selfName {
			return nil
		}
		lines, err := readLines(path)
		if err != nil {
			return err
		}
		found = append(found, check(path, lines)...)
		return nil
	})
	return found, err
}

// check applies the three invariants to one file's lines.
//
// They are deliberately one-directional. Naming the repo's rule, suppressing
// gosec's, or asserting the repo's rule fires all mean the line is a
// credential-shaped value that an external scan reports too, so the registry
// id has to be named as well. The converse is not a violation: the URL
// fixtures trip only the external rule, because their identifiers are not
// credential-shaped, and name only that id.
func check(file string, lines []string) []violation {
	var found []violation
	for i, line := range lines {
		ids := nosemgrepIDs(line)
		if names(ids, repoRuleID) && !names(ids, registryID) {
			found = append(found, violation{file, i + 1, line,
				"nosemgrep directive names " + repoRuleID + " but not " + registryID})
		}
		if nosecG101.MatchString(line) && !namesRegistryIDNear(lines, i) {
			found = append(found, violation{file, i + 1, line,
				"#nosec G101 suppression has no nosemgrep: " + registryID + " within " +
					fmt.Sprint(pairWindow) + " lines"})
		}
		// A fixture positive: the rule is asserted to fire on the next line, so
		// that line is a planted credential an external scan reports too.
		if strings.Contains(line, ruleIDMarker) && i+1 < len(lines) &&
			!names(nosemgrepIDs(lines[i+1]), registryID) {
			found = append(found, violation{file, i + 2, lines[i+1],
				"fixture positive for " + repoRuleID + " does not name " + registryID})
		}
	}
	return found
}

// namesRegistryIDNear reports whether a nosemgrep directive within pairWindow
// lines either side of i names the registry rule id.
//
// The ids are parsed and compared exactly rather than searched for as a
// substring: prose explaining the convention mentions the id without
// suppressing anything, and a substring match would also accept a different
// rule whose id merely starts with this one (gosec.G101-10).
func namesRegistryIDNear(lines []string, i int) bool {
	lo := max(0, i-pairWindow)
	hi := min(len(lines)-1, i+pairWindow)
	for j := lo; j <= hi; j++ {
		if names(nosemgrepIDs(lines[j]), registryID) {
			return true
		}
	}
	return false
}

// nosemgrepIDs returns the rule ids a nosemgrep directive on the line names,
// or nil when it carries none. A reason may follow "--"; ids precede it.
func nosemgrepIDs(line string) []string {
	_, after, found := strings.Cut(line, "nosemgrep:")
	if !found {
		return nil
	}
	if before, _, ok := strings.Cut(after, "--"); ok {
		after = before
	}

	var ids []string
	for _, field := range strings.Split(after, ",") {
		id := strings.Trim(strings.TrimSpace(field), "`")
		// An id never contains whitespace, so anything after the first break
		// is surrounding prose rather than part of the id.
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
func names(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
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
