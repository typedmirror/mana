package evaluator

import (
	"strings"
	"testing"
	"time"

	"github.com/typedmirror/mana/internal/host"
	"github.com/typedmirror/mana/internal/object"
	"github.com/typedmirror/mana/internal/parser"
)

// --- `--` blocks as cells -------------------------------------------------------

// TestStepsFollowTheIntentLines is the reframe the language is built on. A model
// already writes a `--` line before each thing it does; those lines are the
// segmentation, so the run reports one outcome per block rather than one for the
// whole file.
func TestStepsFollowTheIntentLines(t *testing.T) {
	h := host.NewFake()
	h.Shells["echo one"] = host.Shell{Stdout: "one"}
	e, _ := evalSteps(t, h, `-- gather the inputs
@users = [1, 2, 3]

-- check the toolchain
@v = run echo one

-- summarise
@n = @users |> sum`)

	steps := e.Steps()
	if len(steps) != 3 {
		t.Fatalf("got %d blocks, want 3: %+v", len(steps), steps)
	}
	for i, want := range []string{"gather the inputs", "check the toolchain", "summarise"} {
		if steps[i].Intent != want {
			t.Errorf("block %d: got %q, want %q", i, steps[i].Intent, want)
		}
		if steps[i].Status != "ok" {
			t.Errorf("block %d: got status %q", i, steps[i].Status)
		}
	}
}

// TestOnlyTheFailingBlockIsMarked: a model reading the report needs to know
// which of its steps broke, not merely that one did.
func TestOnlyTheFailingBlockIsMarked(t *testing.T) {
	e, _ := evalSteps(t, nil, `-- this one is fine
@a = 1

-- this one is not
@b = read ./absent.json

-- never reached`)
	steps := e.Steps()
	if len(steps) < 2 {
		t.Fatalf("got %+v", steps)
	}
	if steps[0].Status != "ok" {
		t.Errorf("first block: got %q", steps[0].Status)
	}
	if steps[1].Status != "failed" {
		t.Errorf("second block: got %q", steps[1].Status)
	}
	if steps[1].Err == nil || !strings.Contains(steps[1].Err.Reason, "no such file") {
		t.Errorf("the failing block should carry the error: %+v", steps[1].Err)
	}
}

func TestBlocksRecordTheirLine(t *testing.T) {
	e, _ := evalSteps(t, nil, "-- first\n@a = 1\n-- second\n@b = 2")
	steps := e.Steps()
	if steps[0].Line != 1 || steps[1].Line != 3 {
		t.Errorf("lines: got %d and %d", steps[0].Line, steps[1].Line)
	}
}

func TestAFailureBeforeAnyIntentStillGetsABlock(t *testing.T) {
	e, _ := evalSteps(t, nil, "@a = read ./absent.json")
	steps := e.Steps()
	if len(steps) != 1 || steps[0].Status != "failed" {
		t.Fatalf("got %+v", steps)
	}
	if steps[0].Intent != "" {
		t.Errorf("an unlabelled block should say so, not invent a label: %q", steps[0].Intent)
	}
}

// --- run: the three defects -----------------------------------------------------

// TestStderrSurvivesASuccessfulCommand: a command can exit 0 and still say
// something. The value stays the stdout string, but discarding stderr entirely
// would be silent loss inside the runtime.
func TestStderrSurvivesASuccessfulCommand(t *testing.T) {
	h := host.NewFake()
	h.Shells["build"] = host.Shell{Stdout: "artifact", Stderr: "deprecation warning", Code: 0}
	e, v := evalSteps(t, h, "-- building\n@x = run build")
	if v.Inspect() != "artifact" {
		t.Errorf("the value should be stdout: %q", v.Inspect())
	}
	notes := strings.Join(e.Steps()[0].Notes, " | ")
	if !strings.Contains(notes, "deprecation warning") {
		t.Errorf("stderr was dropped: %q", notes)
	}
}

func TestTruncationIsStated(t *testing.T) {
	h := host.NewFake()
	h.Shells["noisy"] = host.Shell{Stdout: "a lot", Truncated: true}
	e, _ := evalSteps(t, h, "-- noisy\n@x = run noisy")
	notes := strings.Join(e.Steps()[0].Notes, " | ")
	if !strings.Contains(notes, "truncated") {
		t.Errorf("a cut that is not reported is a cut that lies: %q", notes)
	}
}

// TestATimeoutSaysSoAndSuggestsTheFix: without a deadline the whole job hangs
// with nothing to show, which is the worst outcome for a caller that fired once
// and is waiting.
func TestATimeoutSaysSoAndSuggestsTheFix(t *testing.T) {
	h := host.NewFake()
	h.Shells["hang"] = host.Shell{TimedOut: true, Code: -1}
	_, v := evalSteps(t, h, "-- this hangs\n@x = run hang")
	err := asErr(t, v)
	if !strings.Contains(err.Reason, "timed out") {
		t.Errorf("got %q", err.Reason)
	}
	if !strings.Contains(err.Suggestion, "--timeout") || !strings.Contains(err.Suggestion, "&") {
		t.Errorf("the error should name both ways out: %q", err.Suggestion)
	}
}

func TestTheTimeoutReachesTheHost(t *testing.T) {
	h := host.NewFake()
	h.Shells["quick"] = host.Shell{Stdout: "ok"}
	e := New(h)
	e.SetTimeout(3 * time.Second)
	runSource(t, e, "@x = run quick")
	if len(h.Ran) != 1 || h.Ran[0].Timeout != 3*time.Second {
		t.Fatalf("got %+v", h.Ran)
	}
}

func TestZeroTimeoutMeansTheDefault(t *testing.T) {
	h := host.NewFake()
	h.Shells["hang"] = host.Shell{TimedOut: true, Code: -1}
	_, v := evalSteps(t, h, "@x = run hang")
	if !strings.Contains(asErr(t, v).Reason, host.DefaultTimeout.String()) {
		t.Errorf("got %q", asErr(t, v).Reason)
	}
}

// --- helpers --------------------------------------------------------------------

func evalSteps(t *testing.T, h *host.Fake, src string) (*Evaluator, object.Value) {
	t.Helper()
	if h == nil {
		h = host.NewFake()
	}
	e := New(h)
	return e, runSource(t, e, src)
}

func runSource(t *testing.T, e *Evaluator, src string) object.Value {
	t.Helper()
	p := parser.New(src)
	prog := p.Parse()
	if errs := p.Errors(); len(errs) > 0 {
		t.Fatalf("parse errors:\n  %s", strings.Join(errs, "\n  "))
	}
	return e.Run(prog)
}

func asErr(t *testing.T, v object.Value) *object.Err {
	t.Helper()
	err, ok := v.(*object.Err)
	if !ok {
		t.Fatalf("expected a failure, got %s(%s)", v.Type(), v.Inspect())
	}
	return err
}
