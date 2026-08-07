package act

import (
	"strings"
	"testing"
	"time"

	"github.com/typedmirror/mana/internal/host"
	"github.com/typedmirror/mana/internal/object"
	"github.com/typedmirror/mana/internal/parser"
)

func plan(t *testing.T, h *host.Fake, src string) (*Plan, *host.Fake) {
	t.Helper()
	if h == nil {
		h = host.NewFake()
	}
	p := parser.New(src)
	prog := p.Parse()
	if errs := p.Errors(); len(errs) > 0 {
		t.Fatalf("parse errors:\n  %s", strings.Join(errs, "\n  "))
	}
	pl, err := DryRun(prog, h)
	if err != nil {
		t.Fatalf("dry run failed: %s", err.Reason)
	}
	return pl, h
}

// --- dry run ------------------------------------------------------------------

// TestDryRunCausesNothing is the whole promise of §14.7. If a dry run can reach
// the host, it is not a dry run.
func TestDryRunCausesNothing(t *testing.T) {
	h := host.NewFake()
	h.Files["./in.json"] = `{"a":1}`
	h.Responses["https://api.com/x"] = "{}"
	h.Shells["rm -rf /tmp/x"] = host.Shell{}
	h.Register("slack", func(host.Call) object.Value { return object.Null{} })

	plan(t, h, `act "destructive" {
    use slack
    @a = fetch https://api.com/x
    @b = read ./in.json
    run rm -rf /tmp/x
    write @a to ./out.json
    create file at ./new.md with "x"
    send "done" to slack channel "ops"
    @c = ask "proceed?"
}`)

	if len(h.Written) != 0 {
		t.Errorf("dry run wrote files: %+v", h.Written)
	}
	if len(h.Posted) != 0 {
		t.Errorf("dry run posted: %+v", h.Posted)
	}
	if len(h.Asked) != 0 {
		t.Errorf("dry run prompted: %+v", h.Asked)
	}
	if h.Stdout.String() != "" {
		t.Errorf("dry run emitted: %q", h.Stdout.String())
	}
}

func TestDryRunListsEveryEffect(t *testing.T) {
	pl, _ := plan(t, nil, `act "work" {
    @a = fetch https://api.com/x
    write @a to ./out.json
    send "done" to output
}`)
	got := pl.Waves[0].Acts[0].Effects
	if len(got) != 3 {
		t.Fatalf("got %v", got)
	}
	for i, want := range []string{"fetch https://api.com/x", "write @a to ./out.json", `send "done" to output`} {
		if got[i] != want {
			t.Errorf("effect %d: got %q, want %q", i, got[i], want)
		}
	}
}

// TestDryRunSeesEffectsInsideBranches: an effect in a match arm still happens,
// so it still has to be reported.
func TestDryRunSeesEffectsInsideBranches(t *testing.T) {
	pl, _ := plan(t, nil, `act "branchy" {
    @r = read ./maybe.json
    @r |> match {
        ok body: write body to ./copy.json
        err msg: send "failed" to output
    }
}`)
	joined := strings.Join(pl.Waves[0].Acts[0].Effects, " | ")
	for _, want := range []string{"read ./maybe.json", "write body to ./copy.json", `send "failed" to output`} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
}

// TestDryRunDoesNotCallAResultSendAnEffect: a bare `send` sets the act's own
// result and reaches nothing outside it.
func TestDryRunDoesNotCallAResultSendAnEffect(t *testing.T) {
	pl, _ := plan(t, nil, `act "quiet" {
    send { computed: 1 }
}`)
	a := pl.Waves[0].Acts[0]
	if len(a.Effects) != 0 {
		t.Errorf("got %v", a.Effects)
	}
	if !a.Produces {
		t.Error("the act does produce a result, and the plan should say so")
	}
	if !strings.Contains(pl.String(), "computes a result only") {
		t.Errorf("rendering:\n%s", pl.String())
	}
}

func TestDryRunGroupsConcurrentActsIntoOneStep(t *testing.T) {
	pl, _ := plan(t, nil, `act "root" { send 1 }
act "left" depends on "root" { send 1 }
act "right" depends on "root" { send 1 }
act "join" depends on "left", "right" { send 1 }`)

	if len(pl.Waves) != 3 {
		t.Fatalf("got %d steps, want 3", len(pl.Waves))
	}
	if len(pl.Waves[1].Acts) != 2 {
		t.Fatalf("step 2 should hold both branches: %+v", pl.Waves[1].Acts)
	}
	if !strings.Contains(pl.String(), "2 acts, concurrent") {
		t.Errorf("rendering should say what runs together:\n%s", pl.String())
	}
}

func TestDryRunRendersModuleCallsWithTheirClauses(t *testing.T) {
	h := host.NewFake()
	h.RegisterWithClauses("inventory", []string{"item"}, func(host.Call) object.Value { return object.Null{} })
	pl, _ := plan(t, h, `act "check" {
    use inventory
    @s = inventory check item "widget-x"
    send @s
}`)
	a := pl.Waves[0].Acts[0]
	if len(a.Effects) != 1 || a.Effects[0] != `inventory.check(item: "widget-x")` {
		t.Errorf("got %v", a.Effects)
	}
	if len(a.Uses) != 1 || a.Uses[0] != "inventory" {
		t.Errorf("the plan should show the permission set: %v", a.Uses)
	}
}

func TestDryRunOfAFlatScript(t *testing.T) {
	pl, _ := plan(t, nil, "-- a flat script\nwrite \"x\" to ./out.json")
	if len(pl.Waves) != 1 || len(pl.Waves[0].Acts) != 1 {
		t.Fatalf("got %+v", pl.Waves)
	}
	if !strings.Contains(pl.String(), "(flat script)") {
		t.Errorf("rendering:\n%s", pl.String())
	}
}

// TestDryRunStillValidatesTheGraph: a job that cannot run should say so before
// anyone is told what it would do.
func TestDryRunStillValidatesTheGraph(t *testing.T) {
	p := parser.New(`act "a" depends on "b" { send 1 }
act "b" depends on "a" { send 2 }`)
	_, err := DryRun(p.Parse(), host.NewFake())
	if err == nil || !strings.Contains(err.Reason, "dependency cycle") {
		t.Fatalf("got %+v", err)
	}
}

// --- retry ---------------------------------------------------------------------

// flaky fails the first n calls, then succeeds.
func flaky(h *host.Fake, name string, failures int) *int {
	calls := 0
	h.Register(name, func(host.Call) object.Value {
		calls++
		if calls <= failures {
			return host.Fail("transient failure %d", calls)
		}
		return object.String("recovered")
	})
	return &calls
}

func TestRetrySucceedsOnASecondAttempt(t *testing.T) {
	h := host.NewFake()
	calls := flaky(h, "svc", 1)
	p := parser.New(`act "a" {
    use svc
    @r = svc ping
    send @r
}`)
	r := RunWith(p.Parse(), h, Options{Retries: 2})
	if !r.OK() {
		t.Fatalf("job failed: %+v", r.Outcomes[0].Err)
	}
	o := outcome(t, r, "a")
	if o.Attempts != 2 {
		t.Errorf("attempts: got %d, want 2", o.Attempts)
	}
	if *calls != 2 {
		t.Errorf("module calls: got %d", *calls)
	}
	if o.Result.Inspect() != "recovered" {
		t.Errorf("got %q", o.Result.Inspect())
	}
}

func TestRetryGivesUpAndReportsTheLastFailure(t *testing.T) {
	h := host.NewFake()
	flaky(h, "svc", 99)
	p := parser.New(`act "a" {
    use svc
    @r = svc ping
    send @r
}`)
	r := RunWith(p.Parse(), h, Options{Retries: 2})
	if r.OK() {
		t.Fatal("job should have failed")
	}
	o := outcome(t, r, "a")
	if o.Attempts != 3 {
		t.Errorf("attempts: got %d, want 3", o.Attempts)
	}
	if !strings.Contains(o.Err.Reason, "transient failure 3") {
		t.Errorf("the last failure should be the reported one: %q", o.Err.Reason)
	}
}

// TestRetryDoesNotReRunDependencies is §14.2: a successful act keeps its result
// for the run, so retrying a failure re-runs only the failure.
func TestRetryDoesNotReRunDependencies(t *testing.T) {
	h := host.NewFake()
	upstream := 0
	h.Register("upstream", func(host.Call) object.Value {
		upstream++
		return object.Number(float64(upstream))
	})
	flaky(h, "downstream", 2)

	p := parser.New(`act "first" {
    use upstream
    @r = upstream go
    send @r
}

act "second" depends on "first" {
    use downstream
    @x = act.first.result
    @r = downstream go
    send @r
}`)
	r := RunWith(p.Parse(), h, Options{Retries: 3})
	if !r.OK() {
		t.Fatalf("job failed: %+v", outcome(t, r, "second").Err)
	}
	if upstream != 1 {
		t.Errorf("the dependency ran %d times; retrying an act must not re-run the graph behind it", upstream)
	}
	if got := outcome(t, r, "second").Attempts; got != 3 {
		t.Errorf("attempts: got %d", got)
	}
}

func TestNoRetryByDefault(t *testing.T) {
	h := host.NewFake()
	calls := flaky(h, "svc", 1)
	p := parser.New(`act "a" {
    use svc
    @r = svc ping
    send @r
}`)
	r := RunWith(p.Parse(), h, Options{})
	if r.OK() {
		t.Fatal("without retries the first failure stands")
	}
	if *calls != 1 {
		t.Errorf("calls: got %d, want 1", *calls)
	}
	if got := outcome(t, r, "a").Attempts; got != 1 {
		t.Errorf("attempts: got %d", got)
	}
}

func TestRetryAppliesToFlatScripts(t *testing.T) {
	h := host.NewFake()
	flaky(h, "svc", 1)
	p := parser.New("use svc\n@r = svc ping\nsend @r")
	r := RunWith(p.Parse(), h, Options{Retries: 1})
	if !r.OK() {
		t.Fatalf("got %+v", r.Outcomes[0].Err)
	}
	if got := r.Outcomes[0].Attempts; got != 2 {
		t.Errorf("attempts: got %d", got)
	}
}

// --- trace ----------------------------------------------------------------------

// TestTraceReportsWhatAnActAlreadyProduces is §14.5's claim: a complete record
// with no instrumentation. Every field here is something the scheduler keeps
// because an act exists, not because tracing was switched on.
func TestTraceReportsWhatAnActAlreadyProduces(t *testing.T) {
	h := host.NewFake()
	h.Register("slack", func(host.Call) object.Value { return object.Null{} })
	p := parser.New(`act "gather" {
    -- pull the day's numbers
    send 1
}

act "notify" depends on "gather" {
    use slack
    -- tell ops
    send err "slack is down"
}`)
	out := Trace(RunWith(p.Parse(), h, Options{}))

	for _, want := range []string{
		"gather", "notify",
		`intent: "pull the day's numbers"`,
		`intent: "tell ops"`,
		"tools:  [slack]",
		"deps:   [gather]",
		"reason: slack is down",
		"FAILED",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("trace is missing %q:\n%s", want, out)
		}
	}
}

func TestTraceShowsSkippedActsAndWhy(t *testing.T) {
	p := parser.New(`act "broken" { send err "boom" }
act "downstream" depends on "broken" { send 1 }`)
	out := Trace(RunWith(p.Parse(), host.NewFake(), Options{}))
	if !strings.Contains(out, "skipped") || !strings.Contains(out, `dependency "broken" failed`) {
		t.Errorf("got:\n%s", out)
	}
}

func TestTraceCountsRetries(t *testing.T) {
	h := host.NewFake()
	flaky(h, "svc", 1)
	p := parser.New("act \"a\" {\n    use svc\n    @r = svc ping\n    send @r\n}")
	out := Trace(RunWith(p.Parse(), h, Options{Retries: 2}))
	if !strings.Contains(out, "tried:  2 times") {
		t.Errorf("got:\n%s", out)
	}
}

func TestShortDurations(t *testing.T) {
	for _, c := range []struct {
		ns   int
		want string
	}{{500, "0ms"}, {5_000_000, "5ms"}, {1_500_000_000, "1.5s"}} {
		if got := short(time.Duration(c.ns)); got != c.want {
			t.Errorf("%d ns: got %q, want %q", c.ns, got, c.want)
		}
	}
}

// TestDryRunShowsADeclaredFailure: `send err` is where a job can deliberately
// stop, which is exactly what a plan reader wants to see.
func TestDryRunShowsADeclaredFailure(t *testing.T) {
	pl, _ := plan(t, nil, `act "guard" {
    send err "preconditions not met"
}`)
	a := pl.Waves[0].Acts[0]
	if a.Produces {
		t.Error("`send err` declares a failure, not a result")
	}
	if len(a.Effects) != 1 || !strings.Contains(a.Effects[0], "send err") {
		t.Errorf("got %v", a.Effects)
	}
}

func TestPlanHasNoTrailingWhitespace(t *testing.T) {
	pl, _ := plan(t, nil, `act "aa" { send 1 }
act "bbbbb" { send 1 }`)
	for _, line := range strings.Split(pl.String(), "\n") {
		if line != strings.TrimRight(line, " ") {
			t.Errorf("trailing space: %q", line)
		}
	}
}
