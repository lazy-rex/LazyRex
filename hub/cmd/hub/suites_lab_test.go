package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// suitesRunElsewhere names each test suite (a tests/<dir> with a lazyrex.json) that another Go test
// of this package runs in its own way, and that test. TestSuitesLab runs every other suite, so a
// new suite is covered with no extra step.
var suitesRunElsewhere = map[string]string{
	"harness":          "TestHarnessSelfTestsPass",
	"harness-fail":     "TestHarnessFailures", // each file must fail with its own reason
	"harness-identity": "TestHarnessIdentityPass",
	"isolation":        "TestHarnessIsolation",
	"peers":            "TestPeersSuitePass",
}

var suiteCounts = regexp.MustCompile(`(?m)^.*: ([0-9]+) passed, ([0-9]+) failed`)

// TestSuitesLab runs `lazyrex-hub test` on each test suite under tests/ that suitesRunElsewhere
// does not name. Each suite must pass: exit 0, at least one test file with tests, and no failure.
func TestSuitesLab(t *testing.T) {
	bin, root := harnessBinary(t)
	funcs := packageTestFuncs(t)
	manifests, _ := filepath.Glob(filepath.Join(root, "tests", "*", "lazyrex.json"))
	var suites []string
	for _, m := range manifests {
		suites = append(suites, filepath.Base(filepath.Dir(m)))
	}
	sort.Strings(suites)
	for name, test := range suitesRunElsewhere {
		if !funcs[test] {
			t.Errorf("tests/%s is skipped for %s, but this package has no such test", name, test)
		}
		if _, err := os.Stat(filepath.Join(root, "tests", name, "lazyrex.json")); err != nil {
			t.Errorf("suitesRunElsewhere names tests/%s, which is not a test suite: %v", name, err)
		}
	}
	ran := 0
	for _, name := range suites {
		if _, ok := suitesRunElsewhere[name]; ok {
			continue
		}
		ran++
		t.Run(name, func(t *testing.T) {
			out, code := runHarness(t, bin, filepath.Join(root, "tests", name), "-keep")
			counts := suiteCounts.FindAllStringSubmatch(out, -1)
			passed := 0
			for _, c := range counts {
				if c[2] != "0" {
					t.Errorf("a file failed: %s", strings.TrimSpace(c[0]))
				}
				if strings.Contains(c[0], ".lua: ") && c[1] != "0" {
					passed++ // a test file, not the parity sweep
				}
			}
			if code != 0 || passed == 0 {
				t.Errorf("exit %d, %d files with passed tests\n%s", code, passed, out)
			}
			assertLabsGone(t, out)
		})
	}
	if ran == 0 {
		t.Fatal("no test suite to run")
	}
}

// packageTestFuncs returns the names of the top-level functions in this package's _test.go files.
func packageTestFuncs(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("*_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no test files: %v", err)
	}
	funcs := map[string]bool{}
	fset := token.NewFileSet()
	for _, f := range files {
		file, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil {
				funcs[fd.Name.Name] = true
			}
		}
	}
	return funcs
}
