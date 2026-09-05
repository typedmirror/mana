package act

import (
	"fmt"
	"sort"
	"strings"

	"github.com/typedmirror/mana/internal/ast"
	"github.com/typedmirror/mana/internal/host"
	"github.com/typedmirror/mana/internal/object"
	"github.com/typedmirror/mana/internal/token"
)

// displayName labels the unnamed act a flat script becomes.
func displayName(name string) string {
	if name == "" {
		return "(flat script)"
	}
	return name
}

// Plan is what a job would do, derived without running it (v2 §14.7).
//
// It is read off the syntax tree, not produced by executing against a recording
// host. That choice matters: a recording host has to invent a return value for
// every read, and the moment a script branches on one the report describes a
// run that will not happen. Reading the tree can only under-report — it shows
// every effect written in an act, and says plainly that arguments are shown as
// written rather than as they will be computed.
type Plan struct {
	Waves []Wave
}

// Wave is a set of acts with no dependency between them. Everything in a wave
// runs at the same time.
type Wave struct {
	Acts []PlannedAct
}

type PlannedAct struct {
	Name     string
	Uses     []string
	Depends  []string
	Effects  []string
	Produces bool // the act sets a result, which is not an effect on anything else
}

// DryRun builds a plan. It causes no effects, and cannot: nothing in here calls
// the host except to load imported act bodies.
func DryRun(prog *ast.Program, h host.Host) (*Plan, *object.Err) {
	acts, loose := Split(prog)
	if len(acts) == 0 {
		body := &ast.Block{}
		for _, s := range loose {
			if u, isUse := s.(*ast.Use); isUse {
				_ = u
				continue
			}
			body.Statements = append(body.Statements, s)
		}
		flat := &ast.Act{Body: body}
		for _, s := range loose {
			if u, isUse := s.(*ast.Use); isUse {
				flat.Uses = append(flat.Uses, u.Module)
			}
		}
		return &Plan{Waves: []Wave{{Acts: []PlannedAct{planOne(flat)}}}}, nil
	}

	if err := checkMixed(loose); err != nil {
		return nil, err
	}
	byName, err := index(acts)
	if err != nil {
		return nil, err
	}
	if err := checkDependencies(acts, byName); err != nil {
		return nil, err
	}
	if err := checkCycles(acts, byName); err != nil {
		return nil, err
	}
	if err := resolveImports(acts, h); err != nil {
		return nil, err
	}

	plan := &Plan{}
	done := map[string]Status{}
	for len(done) < len(acts) {
		ready := readyActs(acts, done)
		if len(ready) == 0 {
			return nil, &object.Err{Reason: "cannot order the acts — the dependency graph is unresolvable"}
		}
		wave := Wave{}
		for _, a := range ready {
			wave.Acts = append(wave.Acts, planOne(a))
			done[a.Name] = Succeeded
		}
		plan.Waves = append(plan.Waves, wave)
	}
	return plan, nil
}

func planOne(a *ast.Act) PlannedAct {
	p := PlannedAct{Name: a.Name, Uses: append([]string(nil), a.Uses...), Depends: a.Depends}
	sort.Strings(p.Uses)
	if a.Body != nil {
		for _, s := range a.Body.Statements {
			walkStatement(s, func(e ast.Expression) {
				if v, isVerb := e.(*ast.Verb); isVerb && setsResult(v) {
					p.Produces = true
					return
				}
				if line := effectOf(e); line != "" {
					p.Effects = append(p.Effects, line)
				}
			})
		}
	}
	return p
}

// setsResult reports whether a send is the act-result form. It reaches nothing
// outside the act, so a dry run should not list it as something that will
// happen to the world.
//
// `send err …` is excluded: it declares a failure rather than a result, and a
// place a job can deliberately stop is exactly what someone reading a plan
// wants to see.
func setsResult(v *ast.Verb) bool {
	if v.Verb != token.SEND {
		return false
	}
	if _, hasTo := v.Clause(token.TO); hasTo {
		return false
	}
	if len(v.Args) > 0 {
		if id, isIdent := v.Args[0].(*ast.Identifier); isIdent && id.Value == "err" {
			return false
		}
	}
	return true
}

// effectOf renders an expression as an effect, or "" if it causes none.
func effectOf(e ast.Expression) string {
	switch n := e.(type) {
	case *ast.Verb:
		return n.String()
	case *ast.ModuleCall:
		return renderModuleCall(n)
	}
	return ""
}

// renderModuleCall prints a module call the way v2 §14.7 shows it:
// `inventory.check(item: "widget-x")`.
func renderModuleCall(n *ast.ModuleCall) string {
	name := n.Module
	if n.Target != "" {
		name += "." + n.Target
	}
	var parts []string
	for _, a := range n.Args {
		parts = append(parts, a.String())
	}
	for _, c := range n.Clauses {
		parts = append(parts, c.Name()+": "+c.Value.String())
	}
	return name + "(" + strings.Join(parts, ", ") + ")"
}

// String renders a plan for a terminal.
func (p *Plan) String() string {
	var b strings.Builder
	b.WriteString("Dry run — nothing was executed\n")
	b.WriteString("Arguments are shown as written; computed values are not evaluated.\n")

	for i, w := range p.Waves {
		concurrent := ""
		if len(w.Acts) > 1 {
			concurrent = fmt.Sprintf(" — %d acts, concurrent", len(w.Acts))
		}
		fmt.Fprintf(&b, "\nstep %d%s\n", i+1, concurrent)
		width := 0
		for _, a := range w.Acts {
			if n := len(displayName(a.Name)); n > width {
				width = n
			}
		}
		for _, a := range w.Acts {
			header := fmt.Sprintf("  %-*s", width, displayName(a.Name))
			if len(a.Uses) > 0 {
				header += "  uses [" + strings.Join(a.Uses, ", ") + "]"
			}
			if len(a.Depends) > 0 {
				header += "  after [" + strings.Join(a.Depends, ", ") + "]"
			}
			b.WriteString(strings.TrimRight(header, " ") + "\n")
			for _, eff := range a.Effects {
				b.WriteString("      would  " + eff + "\n")
			}
			switch {
			case len(a.Effects) == 0 && a.Produces:
				b.WriteString("      no effects — computes a result only\n")
			case len(a.Effects) == 0:
				b.WriteString("      no effects\n")
			}
		}
	}
	return b.String()
}

// --- tree walking ------------------------------------------------------------

// walkStatement visits every expression in a statement, including inside match
// arms and pipe chains, so an effect buried in a branch is still reported.
func walkStatement(s ast.Statement, visit func(ast.Expression)) {
	switch n := s.(type) {
	case *ast.BindStatement:
		walkExpression(n.Value, visit)
	case *ast.ExpressionStatement:
		walkExpression(n.Expression, visit)
	case *ast.Block:
		for _, inner := range n.Statements {
			walkStatement(inner, visit)
		}
	case *ast.Act:
		if n.Body != nil {
			walkStatement(n.Body, visit)
		}
	}
}

func walkExpression(e ast.Expression, visit func(ast.Expression)) {
	if e == nil {
		return
	}
	visit(e)
	switch n := e.(type) {
	case *ast.Verb:
		for _, a := range n.Args {
			walkExpression(a, visit)
		}
		for _, c := range n.Clauses {
			walkExpression(c.Value, visit)
		}
	case *ast.ModuleCall:
		for _, a := range n.Args {
			walkExpression(a, visit)
		}
		for _, c := range n.Clauses {
			walkExpression(c.Value, visit)
		}
	case *ast.Transform:
		walkExpression(n.Arg, visit)
		for _, c := range n.Clauses {
			walkExpression(c.Value, visit)
		}
	case *ast.Pipe:
		walkExpression(n.Left, visit)
		walkExpression(n.Stage, visit)
	case *ast.Fallback:
		walkExpression(n.Left, visit)
		walkExpression(n.Right, visit)
	case *ast.Infix:
		walkExpression(n.Left, visit)
		walkExpression(n.Right, visit)
	case *ast.Prefix:
		walkExpression(n.Right, visit)
	case *ast.Member:
		walkExpression(n.Object, visit)
	case *ast.If:
		walkExpression(n.Cond, visit)
		walkExpression(n.Then, visit)
		walkExpression(n.Else, visit)
	case *ast.ListLiteral:
		for _, el := range n.Elements {
			walkExpression(el, visit)
		}
	case *ast.RecordLiteral:
		for _, pair := range n.Pairs {
			walkExpression(pair.Value, visit)
		}
	case *ast.Match:
		walkExpression(n.Subject, visit)
		for _, arm := range n.Arms {
			if arm.Body != nil {
				walkStatement(arm.Body, visit)
			}
		}
	}
}
