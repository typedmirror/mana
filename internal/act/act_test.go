package act

import (
	"strings"
	"testing"

	"github.com/typedmirror/mana/internal/host"
	"github.com/typedmirror/mana/internal/object"
	"github.com/typedmirror/mana/internal/parser"
)

func run(t *testing.T, h *host.Fake, src string) (*Report, *host.Fake) {
	t.Helper()
	if h == nil {
		h = host.NewFake()
	}
	p := parser.New(src)
	prog := p.Parse()
	if errs := p.Errors(); len(errs) > 0 {
		t.Fatalf("parse errors:\n  %s", strings.Join(errs, "\n  "))
	}
	return Run(prog, h), h
}

// outcome finds one act's record by name.
func outcome(t *testing.T, r *Report, name string) Outcome {
	t.Helper()
	for _, o := range r.Outcomes {
		if o.Name == name {
			return o
		}
	}
	t.Fatalf("no outcome for act %q; got %v", name, order(r))
	return Outcome{}
}

func order(r *Report) []string {
	out := make([]string, len(r.Outcomes))
	for i, o := range r.Outcomes {
		out[i] = o.Name + "=" + string(o.Status)
	}
	return out
}

// --- results -----------------------------------------------------------------

func TestActSendsAResult(t *testing.T) {
	r, _ := run(t, nil, `act "one" {
    send 42
}`)
	o := outcome(t, r, "one")
	if o.Status != Succeeded || !o.HasResult || o.Result.Inspect() != "42" {
		t.Fatalf("got %+v", o)
	}
	if !r.OK() {
		t.Error("job should have succeeded")
	}
}

func TestDependentActReadsTheResult(t *testing.T) {
	r, _ := run(t, nil, `act "produce" {
    send { total: 7 }
}

act "consume" depends on "produce" {
    @p = act.produce.result
    send @p.total
}`)
	if got := outcome(t, r, "consume").Result.Inspect(); got != "7" {
		t.Errorf("got %q", got)
	}
}

// TestHyphenatedActNames is the reason ACTREF is a single token: read as
// ordinary tokens, `act.check-inventory.result` is a subtraction that parses
// cleanly and means something else. Every act name in the spec is hyphenated.
func TestHyphenatedActNames(t *testing.T) {
	r, _ := run(t, nil, `act "check-inventory" {
    send { available: 12 }
}

act "place-order" depends on "check-inventory" {
    @stock = act.check-inventory.result
    send @stock.available
}`)
	if !r.OK() {
		t.Fatalf("job failed: %v", order(r))
	}
	if got := outcome(t, r, "place-order").Result.Inspect(); got != "12" {
		t.Errorf("got %q", got)
	}
}

func TestActWithNoSendHasNoResult(t *testing.T) {
	r, _ := run(t, nil, `act "quiet" {
    @x = 1
}`)
	o := outcome(t, r, "quiet")
	if o.Status != Succeeded {
		t.Fatalf("got %+v", o)
	}
	if o.HasResult {
		t.Error("an act that sent nothing must not report a result")
	}
}

func TestSendingTwiceIsAnError(t *testing.T) {
	r, _ := run(t, nil, `act "two" {
    send 1
    send 2
}`)
	o := outcome(t, r, "two")
	if o.Status != Failed || !strings.Contains(o.Err.Reason, "already sent a result") {
		t.Fatalf("got %+v", o)
	}
}

func TestSendToDestinationStillEmits(t *testing.T) {
	r, h := run(t, nil, `act "emit" {
    send "hello" to output
}`)
	if !r.OK() {
		t.Fatalf("failed: %v", order(r))
	}
	if !strings.Contains(h.Stdout.String(), "hello") {
		t.Errorf("got %q", h.Stdout.String())
	}
	if outcome(t, r, "emit").HasResult {
		t.Error("`send X to output` emits; it must not also set the result")
	}
}

// --- failure -----------------------------------------------------------------

func TestSendErrFailsTheAct(t *testing.T) {
	r, _ := run(t, nil, `act "bad" {
    -- the inventory service is unreachable
    send err "inventory offline"
}`)
	o := outcome(t, r, "bad")
	if o.Status != Failed {
		t.Fatalf("got %+v", o)
	}
	if o.Err.Reason != "inventory offline" {
		t.Errorf("reason: %q", o.Err.Reason)
	}
	if o.Err.Intent != "the inventory service is unreachable" {
		t.Errorf("intent did not reach the act failure: %q", o.Err.Intent)
	}
}

// TestFailedDependencySkipsDependents is I1 at the job level: an act that never
// ran is not a success, and the job is not OK.
func TestFailedDependencySkipsDependents(t *testing.T) {
	r, _ := run(t, nil, `act "first" {
    send err "boom"
}

act "second" depends on "first" {
    send "should not run"
}

act "third" depends on "second" {
    send "also should not run"
}`)
	if outcome(t, r, "first").Status != Failed {
		t.Error("first should have failed")
	}
	for _, name := range []string{"second", "third"} {
		o := outcome(t, r, name)
		if o.Status != Skipped {
			t.Errorf("%s: got %s, want skipped", name, o.Status)
		}
		if !strings.Contains(o.Reason, "dependency") {
			t.Errorf("%s: reason %q should name the dependency", name, o.Reason)
		}
	}
	if r.OK() {
		t.Error("a job with a failed act must not report OK")
	}
}

func TestUnrelatedActStillRunsWhenAnotherFails(t *testing.T) {
	r, _ := run(t, nil, `act "broken" {
    send err "boom"
}

act "fine" {
    send "ok"
}`)
	if outcome(t, r, "fine").Status != Succeeded {
		t.Error("an act with no edge to the failure should still run")
	}
	if r.OK() {
		t.Error("the job as a whole still failed")
	}
}

// --- graph validation --------------------------------------------------------

func TestCycleIsRejectedBeforeAnythingRuns(t *testing.T) {
	// Without this check the scheduler finds nothing ready and stalls — a hang
	// instead of a message.
	r, _ := run(t, nil, `act "a" depends on "b" { send 1 }
act "b" depends on "a" { send 2 }`)
	if r.Err == nil {
		t.Fatal("expected a cycle error")
	}
	if !strings.Contains(r.Err.Reason, "dependency cycle") {
		t.Errorf("got %q", r.Err.Reason)
	}
	if len(r.Outcomes) != 0 {
		t.Error("nothing should have run")
	}
}

func TestLongerCycleIsRejected(t *testing.T) {
	r, _ := run(t, nil, `act "a" depends on "c" { send 1 }
act "b" depends on "a" { send 2 }
act "c" depends on "b" { send 3 }`)
	if r.Err == nil || !strings.Contains(r.Err.Reason, "dependency cycle") {
		t.Fatalf("got %+v", r.Err)
	}
}

func TestSelfDependencyIsRejected(t *testing.T) {
	r, _ := run(t, nil, `act "a" depends on "a" { send 1 }`)
	if r.Err == nil || !strings.Contains(r.Err.Reason, "depends on itself") {
		t.Fatalf("got %+v", r.Err)
	}
}

func TestUnknownDependencyIsRejected(t *testing.T) {
	r, _ := run(t, nil, `act "a" depends on "nope" { send 1 }`)
	if r.Err == nil || !strings.Contains(r.Err.Reason, "not declared") {
		t.Fatalf("got %+v", r.Err)
	}
	if !strings.Contains(r.Err.Suggestion, "a") {
		t.Errorf("the error should list what is declared: %q", r.Err.Suggestion)
	}
}

func TestDuplicateActNamesRejected(t *testing.T) {
	r, _ := run(t, nil, `act "a" { send 1 }
act "a" { send 2 }`)
	if r.Err == nil || !strings.Contains(r.Err.Reason, "two acts are named") {
		t.Fatalf("got %+v", r.Err)
	}
}

// TestReadingANonDependencyIsAnError: the dependency edge is the only thing
// ordering two acts, so reading without one is a race even when a value happens
// to be present.
func TestReadingANonDependencyIsAnError(t *testing.T) {
	r, _ := run(t, nil, `act "a" { send 1 }
act "b" { @x = act.a.result
    send @x }`)
	o := outcome(t, r, "b")
	if o.Status != Failed || !strings.Contains(o.Err.Reason, "not a dependency") {
		t.Fatalf("got %+v", o)
	}
}

func TestMixingActsAndLooseStatementsIsRejected(t *testing.T) {
	r, _ := run(t, nil, `@x = 1
act "a" { send 1 }`)
	if r.Err == nil || !strings.Contains(r.Err.Reason, "either flat or made of acts") {
		t.Fatalf("got %+v", r.Err)
	}
}

func TestTopLevelIntentIsAllowedAlongsideActs(t *testing.T) {
	r, _ := run(t, nil, `-- reasoning about the job as a whole
act "a" { send 1 }`)
	if r.Err != nil {
		t.Fatalf("got %+v", r.Err)
	}
}

// --- isolation and concurrency -----------------------------------------------

// TestActsDoNotShareBindings is the isolation claim from v2 §17.5. Each act has
// its own evaluator, so a binding in one is invisible in another.
func TestActsDoNotShareBindings(t *testing.T) {
	r, _ := run(t, nil, `act "setter" {
    @secret = "visible only here"
    send "done"
}

act "reader" depends on "setter" {
    send @secret
}`)
	o := outcome(t, r, "reader")
	if o.Status != Failed || !strings.Contains(o.Err.Reason, "not bound") {
		t.Fatalf("bindings leaked between acts: %+v", o)
	}
}

func TestActsDoNotShareIntentStacks(t *testing.T) {
	r, _ := run(t, nil, `act "a" {
    -- reasoning belonging to a
    send 1
}

act "b" {
    -- reasoning belonging to b
    send 2
}`)
	for name, want := range map[string]string{"a": "reasoning belonging to a", "b": "reasoning belonging to b"} {
		got := outcome(t, r, name).Intents
		if len(got) != 1 || got[0] != want {
			t.Errorf("act %s: got %v, want exactly [%q]", name, got, want)
		}
	}
}

// TestIndependentActsRunConcurrently exercises the scheduler under -race with
// enough acts that any shared mutable state would be caught.
func TestIndependentActsRunConcurrently(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 24; i++ {
		b.WriteString("act \"w" + string(rune('a'+i%24)) + "\" {\n")
		b.WriteString("    -- independent work\n")
		b.WriteString("    @x = [1, 2, 3] |> sum\n")
		b.WriteString("    send @x\n}\n")
	}
	r, _ := run(t, nil, b.String())
	if !r.OK() {
		t.Fatalf("job failed: %v", order(r))
	}
	if len(r.Outcomes) != 24 {
		t.Fatalf("got %d outcomes, want 24", len(r.Outcomes))
	}
	for _, o := range r.Outcomes {
		if o.Result.Inspect() != "6" {
			t.Errorf("%s: got %q", o.Name, o.Result.Inspect())
		}
	}
}

func TestDiamondGraphOrdering(t *testing.T) {
	// root ─┬→ left  ─┬→ join
	//       └→ right ─┘
	r, _ := run(t, nil, `act "root" { send 1 }
act "left" depends on "root" { send act.root.result }
act "right" depends on "root" { send act.root.result }
act "join" depends on "left", "right" {
    @l = act.left.result
    @r = act.right.result
    send @l + @r
}`)
	if !r.OK() {
		t.Fatalf("job failed: %v", order(r))
	}
	if got := outcome(t, r, "join").Result.Inspect(); got != "2" {
		t.Errorf("got %q", got)
	}
	// root must finish before left and right, which must finish before join.
	pos := map[string]int{}
	for i, o := range r.Outcomes {
		pos[o.Name] = i
	}
	if pos["root"] > pos["left"] || pos["root"] > pos["right"] {
		t.Errorf("root did not complete first: %v", order(r))
	}
	if pos["join"] < pos["left"] || pos["join"] < pos["right"] {
		t.Errorf("join did not complete last: %v", order(r))
	}
}

// --- use / permission boundary -----------------------------------------------

func TestUseRecordsThePermissionSet(t *testing.T) {
	h := host.NewFake()
	h.Register("inventory", func(host.Call) object.Value { return object.Null{} })
	h.Register("slack", func(host.Call) object.Value { return object.Null{} })
	r, _ := run(t, h, `act "reads" {
    use inventory
    send 1
}

act "notifies" depends on "reads" {
    use slack
    send 2
}`)
	if !r.OK() {
		t.Fatalf("job failed: %v", order(r))
	}
	if got := outcome(t, r, "reads").Uses; len(got) != 1 || got[0] != "inventory" {
		t.Errorf("got %v", got)
	}
	if got := outcome(t, r, "notifies").Uses; len(got) != 1 || got[0] != "slack" {
		t.Errorf("got %v — the use set is the permission boundary, so it must not leak", got)
	}
}

// TestUseOfAnAbsentModuleFails: a `use` that quietly did nothing would be a
// permission granted over nothing.
func TestUseOfAnAbsentModuleFails(t *testing.T) {
	r, _ := run(t, nil, `act "a" {
    use postgres
    send 1
}`)
	o := outcome(t, r, "a")
	if o.Status != Failed || !strings.Contains(o.Err.Reason, `no module named "postgres"`) {
		t.Fatalf("got %+v", o)
	}
}

// --- composition -------------------------------------------------------------

func TestActFromFile(t *testing.T) {
	h := host.NewFake()
	h.Files["./jobs/backup.mana"] = "-- nightly backup\n@n = 3\nsend @n"
	r, _ := run(t, h, `act "backup" from ./jobs/backup.mana

act "report" depends on "backup" {
    send act.backup.result
}`)
	if !r.OK() {
		t.Fatalf("job failed: %v %+v", order(r), r.Err)
	}
	if got := outcome(t, r, "report").Result.Inspect(); got != "3" {
		t.Errorf("got %q", got)
	}
}

func TestActFromMissingFileIsReported(t *testing.T) {
	r, _ := run(t, nil, `act "backup" from ./nope.mana`)
	if r.Err == nil || !strings.Contains(r.Err.Reason, "no such file") {
		t.Fatalf("got %+v", r.Err)
	}
}

func TestImportedFileCannotDeclareActs(t *testing.T) {
	h := host.NewFake()
	h.Files["./jobs/nested.mana"] = `act "inner" { send 1 }`
	r, _ := run(t, h, `act "outer" from ./jobs/nested.mana`)
	if r.Err == nil || !strings.Contains(r.Err.Reason, "declares its own acts") {
		t.Fatalf("got %+v", r.Err)
	}
}

// --- flat scripts ------------------------------------------------------------

// TestFlatScriptIsAnImplicitAct: v2 §4.4. A file with no act declarations gets
// the same treatment as one act.
func TestFlatScriptIsAnImplicitAct(t *testing.T) {
	r, h := run(t, nil, `-- a flat script
send "hi" to output`)
	if !r.OK() {
		t.Fatalf("got %+v", r)
	}
	if len(r.Outcomes) != 1 || r.Outcomes[0].Name != "" {
		t.Fatalf("got %v", order(r))
	}
	if !strings.Contains(h.Stdout.String(), "hi") {
		t.Errorf("got %q", h.Stdout.String())
	}
}

// TestBareSendOutsideAnActIsStillAnError is D-012: the new meaning is scoped to
// where a result exists.
func TestBareSendAtTopLevelSetsTheScriptResult(t *testing.T) {
	// A flat script *is* an act, so a bare send sets its result rather than
	// erroring — the destination-less form is only an error where there is no
	// act at all, which cannot happen for a whole file.
	r, _ := run(t, nil, `send 5`)
	if !r.OK() {
		t.Fatalf("got %+v", r)
	}
	if got := r.Outcomes[0]; !got.HasResult || got.Result.Inspect() != "5" {
		t.Errorf("got %+v", got)
	}
}

func TestFlatScriptFailureStillReported(t *testing.T) {
	r, _ := run(t, nil, `@c = read ./nope.json`)
	if r.OK() {
		t.Fatal("a failing flat script must not report OK")
	}
	if r.Outcomes[0].Status != Failed {
		t.Errorf("got %+v", r.Outcomes[0])
	}
}

// --- modules across a graph ---------------------------------------------------

// TestModuleScopingAcrossActs is v2 §7.2's claim in full: the `use` boundary is
// the permission boundary, and it holds per act rather than per job.
func TestModuleScopingAcrossActs(t *testing.T) {
	h := host.NewFake()
	var stockCalls, slackCalls []host.Call
	h.RegisterWithClauses("inventory", []string{"item"}, func(c host.Call) object.Value {
		stockCalls = append(stockCalls, c)
		r := object.NewRecord()
		r.Set("available", object.Number(12))
		return r
	})
	h.RegisterWithClauses("slack", []string{"channel"}, func(c host.Call) object.Value {
		slackCalls = append(slackCalls, c)
		return object.Null{}
	})

	r, _ := run(t, h, `act "check-inventory" {
    use inventory

    -- verify stock levels before the fulfillment run
    @stock = inventory check item "widget-x"
    send @stock
}

act "notify" depends on "check-inventory" {
    use slack

    -- tell ops what we found
    @stock = act.check-inventory.result
    send "available: " + @stock.available to slack channel "ops"
}`)
	if !r.OK() {
		t.Fatalf("job failed: %v %+v", order(r), outcome(t, r, "notify").Err)
	}
	if len(stockCalls) != 1 || stockCalls[0].Target != "check" {
		t.Fatalf("inventory: got %+v", stockCalls)
	}
	if got := stockCalls[0].Intent; got != "verify stock levels before the fulfillment run" {
		t.Errorf("intent did not reach the module: %q", got)
	}
	if len(slackCalls) != 1 || slackCalls[0].Clauses["channel"].Inspect() != "ops" {
		t.Fatalf("slack: got %+v", slackCalls)
	}
	if got := slackCalls[0].Args[0].Inspect(); got != "available: 12" {
		t.Errorf("payload: got %q", got)
	}
}

// TestAnActCannotReachAModuleItDidNotUse is the permission boundary failing
// closed. The module is installed and another act uses it; this one did not.
func TestAnActCannotReachAModuleItDidNotUse(t *testing.T) {
	h := host.NewFake()
	h.Register("postgres", func(host.Call) object.Value { return object.String("rows") })

	r, _ := run(t, h, `act "reads" {
    use postgres
    send postgres query "SELECT 1"
}

act "notifies" depends on "reads" {
    -- no `+"`use postgres`"+` here, so the database is out of reach
    @x = postgres query "SELECT 2"
    send @x
}`)
	if outcome(t, r, "reads").Status != Succeeded {
		t.Fatalf("reads should have succeeded: %+v", outcome(t, r, "reads").Err)
	}
	o := outcome(t, r, "notifies")
	if o.Status != Failed || !strings.Contains(o.Err.Reason, "is not used in this act") {
		t.Fatalf("the use boundary did not hold: %+v", o)
	}
	if o.Err.Intent != "no `use postgres` here, so the database is out of reach" {
		t.Errorf("intent: %q", o.Err.Intent)
	}
}

func TestUsesAreReportedPerAct(t *testing.T) {
	h := host.NewFake()
	h.Register("a", func(host.Call) object.Value { return object.Null{} })
	h.Register("b", func(host.Call) object.Value { return object.Null{} })
	r, _ := run(t, h, `act "one" {
    use a
    send 1
}

act "two" {
    use b
    send 2
}`)
	if got := outcome(t, r, "one").Uses; len(got) != 1 || got[0] != "a" {
		t.Errorf("got %v", got)
	}
	if got := outcome(t, r, "two").Uses; len(got) != 1 || got[0] != "b" {
		t.Errorf("got %v", got)
	}
}
