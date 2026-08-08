package tests

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/typedmirror/mana/internal/host"
	"github.com/typedmirror/mana/internal/repl"
)

// TestReadmeExamplesRun executes every ```mana block in the README.
//
// A README example that does not run teaches something untrue, and this one
// already did: the acts example used `send @x |> f`, which is the exact shape
// the same document lists as a sharp edge two sections later. Parsing was not
// enough to catch it — the block parses and then fails at runtime — so the
// blocks are executed, not merely read.
//
// Fragments that cannot stand alone use a plain fence instead of a `mana` one.
func TestReadmeExamplesRun(t *testing.T) {
	src, err := os.ReadFile("../README.md")
	if err != nil {
		t.Skip("no README")
	}
	blocks := regexp.MustCompile("(?s)```mana\n(.*?)```").FindAllStringSubmatch(string(src), -1)
	if len(blocks) == 0 {
		t.Fatal("no mana blocks found — the extractor is broken, not the README")
	}
	for i, b := range blocks {
		code := strings.TrimSpace(b[1])
		var stdout, stderr strings.Builder
		h := host.NewReal(&stdout, &stderr, strings.NewReader(""))
		if code := repl.RunWith(code, h, repl.Options{Timeout: 5_000_000_000}); code != repl.ExitOK {
			t.Errorf("README block %d exits %d:\n%s\n--- stderr ---\n%s", i+1, code, b[1], stderr.String())
		}
	}
	t.Logf("all %d README examples ran", len(blocks))
}
