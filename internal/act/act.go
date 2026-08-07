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
	Duration  time.Duration
	Uses      []string
	Intents   []string
}

// Report is the whole job.
type Report struct {
	Outcomes []Outcome // in completion-wave order, then alphabetical within a wave
	Err      *object.Err
}

// OK reports whether every act succeeded.
func (r *Report) OK() bool {
	if r.Err != nil {
		return false
	}
	for _, o := range r.Outcomes {
		if o.Status != Succeeded {
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
	acts, loose := Split(prog)
	if len(acts) == 0 {
		return runFlat(prog, h)
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
	return runGraph(acts, h)
}

// runFlat runs a script with no act declarations. It is an act — an unnamed one
// whose body is the file (v2 §4.4) — so it gets the same treatment.
func runFlat(prog *ast.Program, h host.Host) *Report {
	e := evaluator.New(h)
	start := time.Now()
	v := e.Run(prog)
	out := Outcome{Name: "", Status: Succeeded, Duration: time.Since(start), Uses: e.Uses(), Intents: e.Intents()}
	if err, bad := v.(*object.Err); bad {
		out.Status, out.Err = Failed, err
	} else {
		out.Result, out.HasResult = e.Result()
	}
	return &Report{Outcomes: []Outcome{out}}
}

// runGraph resolves the dependency graph and executes it wave by wave.
func runGraph(acts []*ast.Act, h host.Host) *Report {
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
				outcomes[i] = Outcome{Name: a.Name, Status: Skipped, Reason: reason}
				continue
			}
			wg.Add(1)
			go func(i int, a *ast.Act) {
				defer wg.Done()
				outcomes[i] = runOne(a, h, table)
			}(i, a)
		}
		wg.Wait()

		for _, o := range outcomes {
			done[o.Name] = o.Status
			report.Outcomes = append(report.Outcomes, o)
		}
	}
	return report
}

// runOne executes a single act with its own evaluator (v2 §17.5).
func runOne(a *ast.Act, h host.Host, table *Table) Outcome {
	e := evaluator.NewForAct(h, a.Name, a.Depends, table)
	start := time.Now()

	out := Outcome{Name: a.Name, Status: Succeeded}
	for _, u := range a.Uses {
		if v := e.Run(&ast.Program{Statements: []ast.Statement{&ast.Use{Tok: a.Tok, Module: u}}}); object.IsErr(v) {
			out.Status, out.Err = Failed, v.(*object.Err)
			out.Duration, out.Uses, out.Intents = time.Since(start), e.Uses(), e.Intents()
			return out
		}
	}

	var body []ast.Statement
	if a.Body != nil {
		body = a.Body.Statements
	}
	v := e.Run(&ast.Program{Statements: body})
	out.Duration = time.Since(start)
	out.Uses, out.Intents = e.Uses(), e.Intents()

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
