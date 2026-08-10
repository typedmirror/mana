// Package act schedules and runs a job's acts (spec v2 §4, §17.5).
//
// The concurrency model is the dependency graph and nothing else: acts with no
// edge between them run at the same time, and an act waits for exactly the acts
// it named. There are no async keywords in the language because there is
// nothing for them to say.
//
// Isolation is what makes that safe. Each act gets its own evaluator, owning
// its own bindings and its own intent stack, so nothing in the evaluator needs
// a lock. The result table is the only shared structure.
package act

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/typedmirror/mana/internal/ast"
	"github.com/typedmirror/mana/internal/evaluator"
	"github.com/typedmirror/mana/internal/host"
	"github.com/typedmirror/mana/internal/object"
	"github.com/typedmirror/mana/internal/parser"
)

// Table holds completed act results. It is the one structure shared across
// concurrently running acts, so it is the one that needs a lock.
type Table struct {
	mu   sync.RWMutex
	vals map[string]object.Value
}

func NewTable() *Table { return &Table{vals: map[string]object.Value{}} }

func (t *Table) Result(act string) (object.Value, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	v, ok := t.vals[act]
	return v, ok
}

func (t *Table) set(act string, v object.Value) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.vals[act] = v
}

// Status is how one act finished.
type Status string

const (
	// Succeeded means the act ran to completion. It does not mean the act
	// produced a result — an act may legitimately send nothing.
	Succeeded Status = "ok"
	// Failed means the act raised an error, or called `send err`.
	Failed Status = "failed"
	// Skipped means a dependency failed, so this act never ran. It is
	// deliberately not a success: nothing happened, and saying "ok" would be
	// the silent-failure mode this language exists to remove.
	Skipped Status = "skipped"

	// Reused means the act did not run: its text and everything upstream were
	// unchanged since it last succeeded in this session, so its result was
	// restored and its effects were not fired again (D-054). Never reported
	// as "ok" — a reader must be able to see what actually executed.
	Reused Status = "reused"
)

// Outcome is one act's record: what it did, how long it took, what it was
// allowed to touch, and the reasoning it was carrying. Together these are a
// complete trace with no instrumentation added (v2 §14.5).
type Outcome struct {
	Name      string
	Status    Status
	Result    object.Value
	HasResult bool
	Err       *object.Err
	Reason    string // why it was skipped
	Depends   []string
	Attempts  int           // 1 unless the act was retried
	Started   time.Duration // from the start of the job
	Duration  time.Duration
	Uses      []string
	Intents   []string
	Steps     []evaluator.Step // one per `--` block
}

// Report is the whole job.
type Report struct {
	Outcomes []Outcome // in completion-wave order, then alphabetical within a wave
	Err      *object.Err
	Elapsed  time.Duration
}

// Options tune a run.
type Options struct {
	// Timeout bounds each shell command. Zero means the host's default.
	Timeout time.Duration

	// Retries is how many extra attempts a failed act gets. Retrying is cheap
	// because a dependency that already succeeded keeps its result (v2 §14.1,
	// §14.2): only the failed act runs again, never the graph behind it.
	Retries int

	// Prior is the previous job's successful acts, enabling identity-based
	// reuse (D-054). Nil means every act runs.
	Prior *Prior
}

// Prior carries what a finished job established, for the next one to reuse.
type Prior struct {
	acts map[string]priorAct
}

type priorAct struct {
	identity  string
	result    object.Value
	hasResult bool
}

// Remember extracts a Prior from a finished job: every act that succeeded or
// was itself reused, keyed by name, with the exact text that earned the
// result. The dependency graph is the staleness model — identity plus an
// unbroken chain of unchanged ancestors is what "still true" means here.
func Remember(prog *ast.Program, r *Report) *Prior {
	acts, _ := Split(prog)
	byName := map[string]*ast.Act{}
	for _, a := range acts {
		byName[a.Name] = a
	}
	p := &Prior{acts: map[string]priorAct{}}
	for _, o := range r.Outcomes {
		if o.Status != Succeeded && o.Status != Reused {
			continue
		}
		a, ok := byName[o.Name]
		if !ok {
			continue
		}
		p.acts[o.Name] = priorAct{identity: a.String(), result: o.Result, hasResult: o.HasResult}
	}
	return p
}

// reusable computes which acts need not run: identical text to a prior
// success, and every dependency itself reusable. A change anywhere re-runs
// everything downstream of it.
func (p *Prior) reusable(acts []*ast.Act) map[string]bool {
	if p == nil {
		return nil
	}
	ok := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for _, a := range acts {
			if ok[a.Name] {
				continue
			}
			prior, has := p.acts[a.Name]
			if !has || prior.identity != a.String() {
				continue
			}
			deps := true
			for _, d := range a.Depends {
				if !ok[d] {
					deps = false
					break
				}
			}
			if deps {
				ok[a.Name] = true
				changed = true
			}
		}
	}
	return ok
}

// OK reports whether every act succeeded — running counts, and so does an
// unchanged act whose earlier success was reused.
func (r *Report) OK() bool {
	if r.Err != nil {
		return false
	}
	for _, o := range r.Outcomes {
		if o.Status != Succeeded && o.Status != Reused {
			return false
		}
	}
	return true
}

// Split separates act declarations from loose statements.
func Split(prog *ast.Program) (acts []*ast.Act, loose []ast.Statement) {
	for _, s := range prog.Statements {
		if a, ok := s.(*ast.Act); ok {
			acts = append(acts, a)
			continue
		}
		loose = append(loose, s)
	}
	return acts, loose
}

// Run executes a job.
//
// A program is either flat or made of acts. Mixing them is rejected rather than
// guessed at: there is no stated order between a loose statement and an act,
// and inventing one would make the script mean something the spec does not say.
func Run(prog *ast.Program, h host.Host) *Report {
	return RunWith(prog, h, Options{})
}

// RunWith executes a job with options.
func RunWith(prog *ast.Program, h host.Host, opts Options) *Report {
	acts, loose := Split(prog)
	if len(acts) == 0 {
		return runFlat(prog, h, opts)
	}
	for _, s := range loose {
		switch s.(type) {
		case *ast.IntentStatement:
			// A file-level `--` line is reasoning about the job as a whole.
		default:
			return &Report{Err: &object.Err{
				At:         s.String(),
				Line:       s.Line(),
				Reason:     "a script is either flat or made of acts, not both",
				Suggestion: "move this statement into an act, or remove the act declarations",
			}}
		}
	}
	return runGraph(acts, h, opts)
}

// runFlat runs a script with no act declarations. It is an act — an unnamed one
// whose body is the file (v2 §4.4) — so it gets the same treatment.
func runFlat(prog *ast.Program, h host.Host, opts Options) *Report {
	start := time.Now()
	var out Outcome
	for attempt := 0; attempt <= opts.Retries; attempt++ {
		e := evaluator.New(h)
		e.SetTimeout(opts.Timeout)
		v := e.Run(prog)
		out = Outcome{Name: "", Status: Succeeded, Attempts: attempt + 1, Uses: e.Uses(), Intents: e.Intents(), Steps: e.Steps()}
		if err, bad := v.(*object.Err); bad {
			out.Status, out.Err = Failed, err
		} else {
			out.Result, out.HasResult = e.Result()
			break
		}
	}
	out.Duration = time.Since(start)
	return &Report{Outcomes: []Outcome{out}, Elapsed: out.Duration}
}

// runGraph resolves the dependency graph and executes it wave by wave.
func runGraph(acts []*ast.Act, h host.Host, opts Options) *Report {
	byName, err := index(acts)
	if err != nil {
		return &Report{Err: err}
	}
	if err := checkDependencies(acts, byName); err != nil {
		return &Report{Err: err}
	}
	if err := checkCycles(acts, byName); err != nil {
		return &Report{Err: err}
	}
	if err := resolveImports(acts, h); err != nil {
		return &Report{Err: err}
	}

	table := NewTable()
	report := &Report{}
	done := map[string]Status{}
	jobStart := time.Now()
	reuse := opts.Prior.reusable(acts)

	for len(done) < len(acts) {
		ready := readyActs(acts, done)
		if len(ready) == 0 {
			// Guarded by checkCycles; reaching here would mean the graph
			// analysis and the scheduler disagree, which is worth saying out
			// loud rather than hanging.
			return &Report{Err: &object.Err{Reason: "scheduler stalled with acts still pending — the dependency graph and the scheduler disagree"}}
		}

		outcomes := make([]Outcome, len(ready))
		var wg sync.WaitGroup
		for i, a := range ready {
			if reason, blocked := blockedBy(a, done); blocked {
				outcomes[i] = Outcome{Name: a.Name, Status: Skipped, Reason: reason, Depends: a.Depends, Started: time.Since(jobStart)}
				continue
			}
			if reuse[a.Name] {
				// Unchanged since it last succeeded, with unchanged ancestors:
				// restore the result, fire nothing (D-054).
				prior := opts.Prior.acts[a.Name]
				outcomes[i] = Outcome{Name: a.Name, Status: Reused, Result: prior.result, HasResult: prior.hasResult, Depends: a.Depends}
				if prior.hasResult {
					table.set(a.Name, prior.result)
				}
				continue
			}
			wg.Add(1)
			go func(i int, a *ast.Act) {
				defer wg.Done()
				outcomes[i] = runOne(a, h, table, opts, jobStart)
			}(i, a)
		}
		wg.Wait()

		for _, o := range outcomes {
			done[o.Name] = o.Status
			report.Outcomes = append(report.Outcomes, o)
		}
	}
	report.Elapsed = time.Since(jobStart)
	return report
}

// runOne executes a single act, retrying it on failure up to the configured
// limit. Each attempt gets a fresh evaluator — an act that half-ran should not
// see its own leftovers on the way round again.
func runOne(a *ast.Act, h host.Host, table *Table, opts Options, jobStart time.Time) Outcome {
	started := time.Since(jobStart)
	var out Outcome
	for attempt := 0; attempt <= opts.Retries; attempt++ {
		out = attemptOne(a, h, table, opts)
		out.Attempts = attempt + 1
		if out.Status == Succeeded {
			break
		}
	}
	out.Started = started
	out.Duration = time.Since(jobStart) - started
	out.Depends = a.Depends
	return out
}

func attemptOne(a *ast.Act, h host.Host, table *Table, opts Options) Outcome {
	e := evaluator.NewForAct(h, a.Name, a.Depends, table)
	e.SetTimeout(opts.Timeout)
	out := Outcome{Name: a.Name, Status: Succeeded}

	for _, u := range a.Uses {
		if v := e.Run(&ast.Program{Statements: []ast.Statement{&ast.Use{Tok: a.Tok, Module: u}}}); object.IsErr(v) {
			out.Status, out.Err = Failed, v.(*object.Err)
			out.Uses, out.Intents, out.Steps = e.Uses(), e.Intents(), e.Steps()
			return out
		}
	}

	var body []ast.Statement
	if a.Body != nil {
		body = a.Body.Statements
	}
	v := e.Run(&ast.Program{Statements: body})
	out.Uses, out.Intents, out.Steps = e.Uses(), e.Intents(), e.Steps()

	if err, bad := v.(*object.Err); bad {
		out.Status, out.Err = Failed, err
		return out
	}
	if result, sent := e.Result(); sent {
		out.Result, out.HasResult = result, true
		table.set(a.Name, result)
	}
	return out
}

// --- graph analysis ----------------------------------------------------------

func index(acts []*ast.Act) (map[string]*ast.Act, *object.Err) {
	byName := make(map[string]*ast.Act, len(acts))
	for _, a := range acts {
		if _, dup := byName[a.Name]; dup {
			return nil, &object.Err{
				At:     a.String(),
				Line:   a.Line(),
				Reason: fmt.Sprintf("two acts are named %q", a.Name),
			}
		}
		byName[a.Name] = a
	}
	return byName, nil
}

func checkDependencies(acts []*ast.Act, byName map[string]*ast.Act) *object.Err {
	for _, a := range acts {
		for _, d := range a.Depends {
			if d == a.Name {
				return &object.Err{At: a.String(), Line: a.Line(),
					Reason: fmt.Sprintf("act %q depends on itself", a.Name)}
			}
			if _, ok := byName[d]; !ok {
				return &object.Err{At: a.String(), Line: a.Line(),
					Reason:     fmt.Sprintf("act %q depends on %q, which is not declared", a.Name, d),
					Suggestion: "declared acts: " + strings.Join(names(acts), ", ")}
			}
		}
	}
	return nil
}

// checkCycles rejects a dependency loop before anything runs. Without it the
// scheduler would find nothing ready and stall — a hang instead of a message,
// which is the worst way for a job to fail.
func checkCycles(acts []*ast.Act, byName map[string]*ast.Act) *object.Err {
	const (
		visiting = 1
		done     = 2
	)
	state := map[string]int{}
	var stack []string

	var walk func(name string) *object.Err
	walk = func(name string) *object.Err {
		switch state[name] {
		case done:
			return nil
		case visiting:
			cycle := append(append([]string{}, stack[indexOf(stack, name):]...), name)
			return &object.Err{
				At:         byName[name].String(),
				Line:       byName[name].Line(),
				Reason:     "dependency cycle: " + strings.Join(cycle, " → "),
				Suggestion: "an act cannot wait, directly or indirectly, on itself",
			}
		}
		state[name] = visiting
		stack = append(stack, name)
		for _, d := range byName[name].Depends {
			if err := walk(d); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		state[name] = done
		return nil
	}

	for _, a := range acts {
		if err := walk(a.Name); err != nil {
			return err
		}
	}
	return nil
}

// resolveImports loads `act "x" from ./file.mana` bodies (v2 §4.5).
func resolveImports(acts []*ast.Act, h host.Host) *object.Err {
	for _, a := range acts {
		if a.From == "" {
			continue
		}
		src, err := h.ReadFile(a.From)
		if err != nil {
			return &object.Err{At: a.String(), Line: a.Line(),
				Reason: fmt.Sprintf("act %q: %v", a.Name, err)}
		}
		p := parser.New(src)
		prog := p.Parse()
		if errs := p.Errors(); len(errs) > 0 {
			return &object.Err{At: a.String(), Line: a.Line(),
				Reason: fmt.Sprintf("act %q: %s does not parse: %s", a.Name, a.From, errs[0])}
		}
		inner, loose := Split(prog)
		if len(inner) > 0 {
			return &object.Err{At: a.String(), Line: a.Line(),
				Reason:     fmt.Sprintf("act %q: %s declares its own acts", a.Name, a.From),
				Suggestion: "an imported file is a flat script; acts cannot nest"}
		}
		body := &ast.Block{Tok: a.Tok}
		for _, s := range loose {
			if u, isUse := s.(*ast.Use); isUse {
				a.Uses = append(a.Uses, u.Module)
				continue
			}
			body.Statements = append(body.Statements, s)
		}
		a.Body = body
	}
	return nil
}

// readyActs returns the acts whose dependencies have all finished, in a stable
// order so a job's trace reads the same way twice.
func readyActs(acts []*ast.Act, done map[string]Status) []*ast.Act {
	var ready []*ast.Act
	for _, a := range acts {
		if _, finished := done[a.Name]; finished {
			continue
		}
		if allSettled(a.Depends, done) {
			ready = append(ready, a)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].Name < ready[j].Name })
	return ready
}

func allSettled(deps []string, done map[string]Status) bool {
	for _, d := range deps {
		if _, ok := done[d]; !ok {
			return false
		}
	}
	return true
}

// blockedBy reports whether a dependency failed or was itself skipped.
func blockedBy(a *ast.Act, done map[string]Status) (string, bool) {
	for _, d := range a.Depends {
		switch done[d] {
		case Failed:
			return fmt.Sprintf("dependency %q failed", d), true
		case Skipped:
			return fmt.Sprintf("dependency %q was skipped", d), true
		}
	}
	return "", false
}

func names(acts []*ast.Act) []string {
	out := make([]string, len(acts))
	for i, a := range acts {
		out[i] = a.Name
	}
	sort.Strings(out)
	return out
}

func indexOf(xs []string, want string) int {
	for i, x := range xs {
		if x == want {
			return i
		}
	}
	return 0
}
