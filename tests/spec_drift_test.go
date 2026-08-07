package tests

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/typedmirror/mana/internal/parser"
)

// specPath is where the language specification lives. It is deliberately
// outside version control, which is exactly why this test exists: nothing else
// would notice the document and the implementation drifting apart.
const specPath = "../docs/spec.md"

// TestSpecExamplesStillParse extracts every ```mana block from the spec and
// parses it. A spec that contains a program the implementation cannot read is a
// spec that has drifted, and the failure names the block.
//
// It skips when the spec is absent so a clean checkout still passes.
func TestSpecExamplesStillParse(t *testing.T) {
	src, err := os.ReadFile(specPath)
	if err != nil {
		t.Skipf("no spec at %s — nothing to check against", specPath)
	}

	blocks := regexp.MustCompile("(?s)```mana\n(.*?)```").FindAllStringSubmatch(string(src), -1)
	if len(blocks) == 0 {
		t.Fatalf("%s has no ```mana blocks — the extractor is broken, not the spec", specPath)
	}

	// Every block is checked. There is deliberately no allowlist for
	// "illustrative fragments": each one the spec tags as `mana` is a claim
	// about what the language accepts, and an exemption is where a real
	// divergence would hide.
	for i, b := range blocks {
		code := strings.TrimSpace(b[1])
		p := parser.New(code)
		p.Parse()
		if errs := p.Errors(); len(errs) > 0 {
			t.Errorf("spec block %d does not parse:\n%s\n  %s", i+1, code, strings.Join(errs, "\n  "))
		}
	}
	t.Logf("all %d spec blocks parse", len(blocks))
}

// TestSpecCompleteExampleIsMirroredInTests guards the strongest check we have.
// The §14 program is embedded verbatim in the parser and evaluator suites; if
// the spec's copy changes and those do not, the suites stop testing the spec
// and nothing else would say so.
func TestSpecCompleteExampleIsMirroredInTests(t *testing.T) {
	spec, err := os.ReadFile(specPath)
	if err != nil {
		t.Skipf("no spec at %s", specPath)
	}
	parts := strings.SplitN(string(spec), "## 14. Complete Example", 2)
	if len(parts) != 2 {
		t.Fatal("§14 not found in the spec")
	}
	m := regexp.MustCompile("(?s)```mana\n(.*?)```").FindStringSubmatch(parts[1])
	if m == nil {
		t.Fatal("§14 has no mana block")
	}
	want := strings.TrimSpace(m[1])

	for _, f := range []string{
		"../internal/parser/parser_test.go",
		"../internal/evaluator/evaluator_test.go",
	} {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), want) {
			t.Errorf("%s no longer contains spec §14 verbatim — update it, or the suite is testing a program the spec does not have", f)
		}
	}

	p := parser.New(want)
	p.Parse()
	if errs := p.Errors(); len(errs) > 0 {
		t.Errorf("spec §14 does not parse:\n  %s", strings.Join(errs, "\n  "))
	}
}
