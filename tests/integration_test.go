// Package tests runs the .mana scripts in this directory end to end.
//
// These are the only tests that exercise the real host — a real shell, a real
// filesystem — so they are the check that the fakes have not drifted from what
// the machine actually does. Every script is written to be independent of the
// working directory, so `./bin/mana tests/<name>.mana` from the repo root and
// `go test ./tests/` agree.
package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/typedmirror/mana/internal/host"
	"github.com/typedmirror/mana/internal/repl"
)

type script struct {
	name string
	exit int
	// errContains are substrings the diagnostic stream must carry. Checking the
	// error text here rather than in a golden file keeps the assertion about
	// the error *model* rather than about formatting.
	errContains []string
}

var scripts = []script{
	{name: "fallback_chain", exit: repl.ExitOK},
	{name: "pipe_transform", exit: repl.ExitOK},
	{name: "match_dispatch", exit: repl.ExitOK},
	{name: "act_graph", exit: repl.ExitOK},
	{name: "env_run", exit: repl.ExitOK},
	{
		name: "act_failure",
		exit: repl.ExitRuntime,
		errContains: []string{
			`act broken:`,
			`intent: "the upstream service is unreachable"`,
			`reason: "inventory offline"`,
			`act downstream: skipped — dependency "broken" failed`,
		},
	},
	{
		name: "error_model",
		exit: repl.ExitRuntime,
		errContains: []string{
			"status: err",
			`at: "read ./definitely-absent.json"`,
			`intent: "reading a file that was never going to be there"`,
			"reason:",
			"no such file or directory",
		},
	},
	{
		name:        "intent_stack",
		exit:        repl.ExitRuntime,
		errContains: []string{`intent: "this is the entry that should appear in the error below"`},
	},
}

func TestScripts(t *testing.T) {
	for _, s := range scripts {
		t.Run(s.name, func(t *testing.T) {
			path := s.name + ".mana"
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			var stdout, stderr strings.Builder
			h := host.NewReal(&stdout, &stderr, strings.NewReader(""))
			code := repl.Run(string(src), h)

			if code != s.exit {
				t.Errorf("exit %d, want %d\nstderr:\n%s", code, s.exit, stderr.String())
			}
			for _, want := range s.errContains {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("diagnostics missing %q:\n%s", want, stderr.String())
				}
			}
			compareGolden(t, s.name+".expected", stdout.String())
		})
	}
}

// compareGolden checks stdout against a committed golden file. Run with
// -update to rewrite them after an intentional change.
func compareGolden(t *testing.T, path, got string) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v (run `go test ./tests/ -update` to create it)", filepath.Base(path), err)
	}
	if got != string(want) {
		t.Errorf("stdout does not match %s\ngot:\n%s\nwant:\n%s", filepath.Base(path), got, want)
	}
}

// TestDiagnosticsNeverReachStdout is the separation the exit code relies on:
// piping a script's output somewhere must never mix in the runtime's own
// commentary about it.
//
// Note this is not "a failing script writes nothing" — in an act graph one act
// can succeed and emit while another fails, and that partial output is correct.
// The invariant is about which stream, not about emptiness.
func TestDiagnosticsNeverReachStdout(t *testing.T) {
	for _, s := range scripts {
		if s.exit == repl.ExitOK {
			continue
		}
		src, err := os.ReadFile(s.name + ".mana")
		if err != nil {
			t.Fatal(err)
		}
		var stdout, stderr strings.Builder
		repl.Run(string(src), host.NewReal(&stdout, &stderr, strings.NewReader("")))
		for _, leak := range []string{"status: err", "skipped —", "parse error"} {
			if strings.Contains(stdout.String(), leak) {
				t.Errorf("%s leaked %q onto stdout: %q", s.name, leak, stdout.String())
			}
		}
	}
}
