package host

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/typedmirror/mana/internal/object"
)

// stub writes an executable that stands in for the claude CLI (D-047) and
// returns a Claude wired to it.
func stub(t *testing.T, script string) *Claude {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude-stub")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Claude{cmd: path, timeout: DefaultTimeout}
}

func mustErr(t *testing.T, v object.Value) *object.Err {
	t.Helper()
	bad, ok := v.(*object.Err)
	if !ok {
		t.Fatalf("wanted an error, got %s: %s", v.Type(), v.Inspect())
	}
	return bad
}

// --- failure paths first -----------------------------------------------------

func TestClaudeRejectsATarget(t *testing.T) {
	c := stub(t, "echo never-reached")
	bad := mustErr(t, c.Execute(Call{Target: "summarize", Args: []object.Value{object.String("x")}}))
	if !strings.Contains(bad.Reason, `"summarize"`) || bad.Suggestion == "" {
		t.Errorf("got %+v", bad)
	}
}

func TestClaudeNeedsAPrompt(t *testing.T) {
	c := stub(t, "echo never-reached")
	if bad := mustErr(t, c.Execute(Call{})); bad.Suggestion == "" {
		t.Errorf("no-args: %+v", bad)
	}
	bad := mustErr(t, c.Execute(Call{Args: []object.Value{object.Number(7)}}))
	if !strings.Contains(bad.Reason, "number") {
		t.Errorf("non-string: %+v", bad)
	}
}

func TestClaudeFailureCarriesExitAndStderr(t *testing.T) {
	c := stub(t, `echo "quota exhausted, try later" >&2; exit 3`)
	bad := mustErr(t, c.Execute(Call{Args: []object.Value{object.String("hi")}}))
	if !strings.Contains(bad.Reason, "exited 3") || !strings.Contains(bad.Reason, "quota exhausted") {
		t.Errorf("reason: %q", bad.Reason)
	}
}

func TestClaudeEmptyReplyIsAnError(t *testing.T) {
	c := stub(t, "exit 0")
	bad := mustErr(t, c.Execute(Call{Args: []object.Value{object.String("hi")}}))
	if !strings.Contains(bad.Reason, "returned nothing") {
		t.Errorf("reason: %q", bad.Reason)
	}
}

func TestClaudeTimesOut(t *testing.T) {
	c := stub(t, "sleep 5; echo too-late")
	c.timeout = 50 * time.Millisecond
	bad := mustErr(t, c.Execute(Call{Args: []object.Value{object.String("hi")}}))
	if !strings.Contains(bad.Reason, "timed out") {
		t.Errorf("reason: %q", bad.Reason)
	}
}

func TestClaudeRejectsUnknownFormat(t *testing.T) {
	c := stub(t, "echo hello")
	bad := mustErr(t, c.Execute(Call{
		Args:    []object.Value{object.String("hi")},
		Clauses: map[string]object.Value{"as": object.Word("xml")},
	}))
	if !strings.Contains(bad.Suggestion, "text") || !strings.Contains(bad.Suggestion, "json") {
		t.Errorf("suggestion: %q", bad.Suggestion)
	}
}

func TestClaudeProseWhereJSONWasAskedIsHard(t *testing.T) {
	c := stub(t, `echo "Sure! Here is the JSON you wanted: {}"`)
	bad := mustErr(t, c.Execute(Call{
		Args:    []object.Value{object.String("hi")},
		Clauses: map[string]object.Value{"as": object.Word("json")},
	}))
	if !strings.Contains(bad.Reason, "not the json") {
		t.Errorf("reason: %q", bad.Reason)
	}
}

func TestClaudeMissingBinaryPointsAtTheOverride(t *testing.T) {
	c := &Claude{cmd: "/definitely/not/here", timeout: DefaultTimeout}
	bad := mustErr(t, c.Execute(Call{Args: []object.Value{object.String("hi")}}))
	if !strings.Contains(bad.Suggestion, "MANA_CLAUDE_CMD") {
		t.Errorf("suggestion: %q", bad.Suggestion)
	}
}

// The module must never pre-set Intent: adopt() fills it only when empty, so a
// module that set it would silently suppress the automatic attachment (D-045).
func TestClaudeErrorsLeaveIntentEmpty(t *testing.T) {
	c := stub(t, "exit 1")
	bad := mustErr(t, c.Execute(Call{Args: []object.Value{object.String("hi")}, Intent: "checking"}))
	if bad.Intent != "" {
		t.Errorf("module pre-set Intent to %q", bad.Intent)
	}
}

// --- the working half --------------------------------------------------------

func TestClaudeReturnsTextWithOneNewlineTrimmed(t *testing.T) {
	c := stub(t, `printf 'two words\n'`)
	v := c.Execute(Call{Args: []object.Value{object.String("hi")}})
	if s, ok := v.(object.String); !ok || string(s) != "two words" {
		t.Errorf("got %s: %q", v.Type(), v.Inspect())
	}
}

func TestClaudeParsesJSON(t *testing.T) {
	c := stub(t, `echo '{ "verdict": "pass", "score": 9 }'`)
	v := c.Execute(Call{
		Args:    []object.Value{object.String("hi")},
		Clauses: map[string]object.Value{"as": object.Word("json")},
	})
	rec, ok := v.(*object.Record)
	if !ok {
		t.Fatalf("got %s: %s", v.Type(), v.Inspect())
	}
	if got, _ := rec.Get("verdict"); got.Inspect() != "pass" {
		t.Errorf("verdict: %v", got)
	}
}

// Hermes wave-2 sharp edge: a flag-shaped model value would land on argv as
// --model's value; if the CLI's parser ever mis-read it, that is argv-shaped
// injection. Rejected at the module boundary instead.
func TestClaudeRejectsFlagShapedModel(t *testing.T) {
	c := stub(t, "cat")
	with := object.NewRecord()
	with.Set("model", object.String("--dangerously-skip-permissions"))
	bad := mustErr(t, c.Execute(Call{
		Args:    []object.Value{object.String("hi")},
		Clauses: map[string]object.Value{"with": with},
	}))
	if !strings.Contains(bad.Reason, "model") {
		t.Errorf("reason: %q", bad.Reason)
	}
}

// The full outgoing shape: argv carries the flags, the model, and the intent;
// stdin carries the prompt with the `with` values as labelled blocks.
func TestClaudeAssemblesTheCall(t *testing.T) {
	c := stub(t, `printf '%s\n' "$@"; cat`)
	with := object.NewRecord()
	with.Set("model", object.String("opus"))
	with.Set("diff", object.String("-old\n+new"))
	v := c.Execute(Call{
		Args:    []object.Value{object.String("review this")},
		Clauses: map[string]object.Value{"with": with},
		Intent:  "second opinion before shipping",
	})
	out, ok := v.(object.String)
	if !ok {
		t.Fatalf("got %s: %s", v.Type(), v.Inspect())
	}
	for _, want := range []string{
		"--safe-mode",
		"--tools",
		"--no-session-persistence",
		"--model\nopus",
		"--append-system-prompt\nCaller intent: second opinion before shipping",
		"review this",
		"diff:\n-old\n+new",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(string(out), "model:") {
		t.Error("model leaked into the prompt as a context block")
	}
}

func TestClaudeStructuredWithValueBecomesJSON(t *testing.T) {
	c := stub(t, "cat")
	items := &object.List{Elements: []object.Value{object.Number(1), object.Number(2)}}
	v := c.Execute(Call{
		Args:    []object.Value{object.String("count these")},
		Clauses: map[string]object.Value{"with": items},
	})
	out := v.(object.String)
	if !strings.Contains(string(out), "input:\n[1, 2]") && !strings.Contains(string(out), "input:\n[1,2]") {
		t.Errorf("got:\n%s", out)
	}
}

func TestNewClaudeResolvesRelativeOverrides(t *testing.T) {
	t.Setenv("MANA_CLAUDE_CMD", "tests/claude_stub.sh")
	c := NewClaude()
	if !filepath.IsAbs(c.cmd) {
		t.Errorf("relative override not resolved: %q", c.cmd)
	}
	t.Setenv("MANA_CLAUDE_CMD", "")
	if c := NewClaude(); c.cmd != "claude" {
		t.Errorf("default: %q", c.cmd)
	}
}

// Concurrent acts call Execute on one module value; the race detector is the
// assertion.
func TestClaudeIsSafeConcurrently(t *testing.T) {
	c := stub(t, "cat")
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v := c.Execute(Call{Args: []object.Value{object.String("hello")}})
			if object.IsErr(v) {
				t.Errorf("unexpected failure: %s", v.Inspect())
			}
		}()
	}
	wg.Wait()
}
