// Package evaluator walks a Mana tree and executes it.
//
// Two things distinguish it from a textbook tree-walker, and both come straight
// from the spec:
//
//   - It carries an intent stack (§15.3). Every `--` line pushes onto it, and
//     every failure is stamped with the most recent entry. That is the mechanism
//     that turns the agent's reasoning into a diagnostic.
//   - It never panics and never returns a Go error to the script. Failures are
//     *object.Err values (§10), so `or` can catch them and `match` can dispatch
//     on them.
package evaluator

import (
	"fmt"

	"github.com/typedmirror/mana/internal/ast"
	"github.com/typedmirror/mana/internal/host"
	"github.com/typedmirror/mana/internal/object"
)

// Evaluator holds the whole runtime state of a script.
type Evaluator struct {
	host      host.Host
	binds     map[string]object.Value
	bindOrder []string // insertion order, so an unhandled failure is reported deterministically
	intents   []string

	// OnIntent, when set, is called for each `--` line as it executes. The
	// REPL uses it to show reasoning flowing past; nothing else needs it,
	// which is why the evaluator does not write to a stream itself.
	OnIntent func(string)
}

// New returns an Evaluator that will cause its effects through h.
func New(h host.Host) *Evaluator {
	return &Evaluator{host: h, binds: map[string]object.Value{}}
}

// Intents returns the reasoning collected so far, oldest first. Exposed because
// the stack is a product feature, not an implementation detail.
func (e *Evaluator) Intents() []string { return e.intents }

// scope holds bare-identifier bindings. Only two things create one: a match arm
// binder and an element-wise transform. Axiom 3 says the LLM does not manage
// resources, so there are no scoping variants beyond these.
type scope struct {
	parent  *scope
	names   map[string]object.Value
	self    object.Value
	hasSelf bool
}

func (s *scope) child() *scope { return &scope{parent: s, names: map[string]object.Value{}} }

func (s *scope) withSelf(v object.Value) *scope {
	c := s.child()
	c.self, c.hasSelf = v, true
	return c
}

func (s *scope) bind(name string, v object.Value) {
	if s.names == nil {
		s.names = map[string]object.Value{}
	}
	s.names[name] = v
}

func (s *scope) lookup(name string) (object.Value, bool) {
	for cur := s; cur != nil; cur = cur.parent {
		if v, ok := cur.names[name]; ok {
			return v, true
		}
	}
	return nil, false
}

func (s *scope) selfValue() (object.Value, bool) {
	for cur := s; cur != nil; cur = cur.parent {
		if cur.hasSelf {
			return cur.self, true
		}
	}
	return nil, false
}

// --- driving -----------------------------------------------------------------

// Run executes a whole program and returns its last value, or the failure that
// stopped it.
//
// Binding a failure is legal — errors are data (§10), and spec §10.2's second
// mechanism binds a failed fetch and then matches on it. So a bind never halts.
// Everything else does: an error reaching a bare statement is unhandled, and
// running past it is the silent-failure mode the language exists to remove.
//
// The closing sweep covers the remaining gap. A script that binds a failure and
// then simply forgets about it would otherwise exit clean, which is the same
// defect one statement further along.
func (e *Evaluator) Run(prog *ast.Program) object.Value {
	root := &scope{names: map[string]object.Value{}}
	last := e.runStatements(prog.Statements, root)
	if object.IsErr(last) {
		return last
	}
	if err := e.unhandled(); err != nil {
		return err
	}
	return last
}

func (e *Evaluator) runStatements(stmts []ast.Statement, sc *scope) object.Value {
	var last object.Value = object.Null{}
	for _, st := range stmts {
		last = e.evalStatement(st, sc)
		if _, isBind := st.(*ast.BindStatement); isBind {
			continue
		}
		if object.IsErr(last) {
			return last
		}
	}
	return last
}

// unhandled returns the first bound failure that nothing took responsibility
// for, in binding order.
func (e *Evaluator) unhandled() object.Value {
	for _, name := range e.bindOrder {
		if err, bad := e.binds[name].(*object.Err); bad && !err.Handled() {
			return err
		}
	}
	return nil
}

func (e *Evaluator) evalStatement(node ast.Statement, sc *scope) object.Value {
	switch s := node.(type) {
	case *ast.IntentStatement:
		e.intents = append(e.intents, s.Text)
		if e.OnIntent != nil {
			e.OnIntent(s.Text)
		}
		return object.Null{}
	case *ast.BindStatement:
		v := e.eval(s.Value, sc)
		if _, seen := e.binds[s.Name]; !seen {
			e.bindOrder = append(e.bindOrder, s.Name)
		}
		e.binds[s.Name] = v
		return v
	case *ast.ExpressionStatement:
		return e.eval(s.Expression, sc)
	case *ast.Block:
		return e.runStatements(s.Statements, sc)
	}
	return e.fail(node, "unknown statement %T", node)
}

// fail builds an error stamped with where it happened and the reasoning that
// preceded it. Every error in the evaluator goes through here so no failure can
// reach the user without its intent.
func (e *Evaluator) fail(node ast.Node, format string, args ...any) *object.Err {
	err := &object.Err{
		At:     node.String(),
		Reason: fmt.Sprintf(format, args...),
		Line:   node.Line(),
	}
	if n := len(e.intents); n > 0 {
		err.Intent = e.intents[n-1]
	}
	return err
}

// adopt stamps an error raised deeper down — by a host call or a helper — with
// this node's position and the current intent.
func (e *Evaluator) adopt(node ast.Node, err *object.Err) *object.Err {
	if err.At == "" {
		err.At = node.String()
	}
	if err.Line == 0 {
		err.Line = node.Line()
	}
	if err.Intent == "" && len(e.intents) > 0 {
		err.Intent = e.intents[len(e.intents)-1]
	}
	return err
}

// --- expressions -------------------------------------------------------------

func (e *Evaluator) eval(node ast.Expression, sc *scope) object.Value {
	switch n := node.(type) {
	case *ast.NumberLiteral:
		return object.Number(n.Value)
	case *ast.StringLiteral:
		return object.String(n.Value)
	case *ast.BooleanLiteral:
		return object.Bool(n.Value)
	case *ast.Identifier:
		return e.evalIdentifier(n, sc)
	case *ast.Binding:
		v, ok := e.binds[n.Name]
		if !ok {
			return e.fail(n, "@%s is not bound", n.Name)
		}
		return v
	case *ast.Self:
		v, ok := sc.selfValue()
		if !ok {
			return e.fail(n, "@ is only meaningful inside a transform")
		}
		return v
	case *ast.ListLiteral:
		return e.evalList(n, sc)
	case *ast.RecordLiteral:
		return e.evalRecord(n, sc)
	case *ast.Prefix:
		return e.evalPrefix(n, sc)
	case *ast.Infix:
		return e.evalInfix(n, sc)
	case *ast.Member:
		return e.evalMember(n, sc)
	case *ast.Fallback:
		return e.evalFallback(n, sc)
	case *ast.Pipe:
		return e.evalPipe(n, sc)
	case *ast.If:
		return e.evalIf(n, sc)
	case *ast.Verb:
		return e.evalVerb(n, sc, nil)
	case *ast.Transform:
		return e.fail(n, "%s needs an input — pipe a value into it", n.Name)
	case *ast.Match:
		return e.fail(n, "match needs a subject — pipe a value into it")
	}
	return e.fail(node, "unknown expression %T", node)
}

// evalIdentifier resolves a bare word. Inside a transform an unresolved name can
// only have meant a field of the current element, so it fails there rather than
// degrading into a Word — otherwise `where stauts is "active"` would compare a
// typo to a string and quietly filter everything out.
func (e *Evaluator) evalIdentifier(n *ast.Identifier, sc *scope) object.Value {
	if v, ok := sc.lookup(n.Value); ok {
		return v
	}
	if self, ok := sc.selfValue(); ok {
		if rec, isRec := self.(*object.Record); isRec {
			if v, has := rec.Get(n.Value); has {
				return v
			}
			return e.fail(n, "no field %q on this element (it has %v)", n.Value, rec.Keys())
		}
		return e.fail(n, "cannot read field %q from a %s element", n.Value, self.Type())
	}
	if n.Value == "context" {
		return e.contextRecord()
	}
	return object.Word(n.Value)
}

// contextRecord materializes the ambient environment (spec §12).
func (e *Evaluator) contextRecord() *object.Record {
	c := e.host.Context()
	env := object.NewRecord()
	env.Set("cwd", object.String(c.Cwd))
	env.Set("os", object.String(c.OS))
	env.Set("today", object.String(c.Today))
	env.Set("now", object.String(c.Now))

	msgs := &object.List{}
	for _, m := range c.Messages {
		msgs.Elements = append(msgs.Elements, object.String(m))
	}

	r := object.NewRecord()
	r.Set("user", object.String(c.User))
	r.Set("last_message", object.String(c.LastMessage))
	r.Set("messages", msgs)
	r.Set("env", env)
	return r
}

func (e *Evaluator) evalList(n *ast.ListLiteral, sc *scope) object.Value {
	out := &object.List{Elements: make([]object.Value, 0, len(n.Elements))}
	for _, el := range n.Elements {
		v := e.eval(el, sc)
		if object.IsErr(v) {
			return v
		}
		out.Elements = append(out.Elements, v)
	}
	return out
}

func (e *Evaluator) evalRecord(n *ast.RecordLiteral, sc *scope) object.Value {
	out := object.NewRecord()
	for _, p := range n.Pairs {
		v := e.eval(p.Value, sc)
		if object.IsErr(v) {
			return v
		}
		out.Set(p.Key, v)
	}
	return out
}

func (e *Evaluator) evalPrefix(n *ast.Prefix, sc *scope) object.Value {
	right := e.eval(n.Right, sc)
	if object.IsErr(right) {
		return right
	}
	num, ok := right.(object.Number)
	if !ok {
		return e.fail(n, "cannot negate a %s", right.Type())
	}
	return -num
}

func (e *Evaluator) evalMember(n *ast.Member, sc *scope) object.Value {
	obj := e.eval(n.Object, sc)
	if object.IsErr(obj) {
		return obj
	}
	rec, ok := obj.(*object.Record)
	if !ok {
		return e.fail(n, "cannot read field %q from a %s", n.Property, obj.Type())
	}
	v, has := rec.Get(n.Property)
	if !has {
		return e.fail(n, "no field %q (it has %v)", n.Property, rec.Keys())
	}
	return v
}

// evalFallback is `or` (spec §8.3). It catches a failure and tries the next
// option; first success wins.
func (e *Evaluator) evalFallback(n *ast.Fallback, sc *scope) object.Value {
	left := e.eval(n.Left, sc)
	err, bad := left.(*object.Err)
	if !bad {
		return left
	}
	err.Handle()
	return e.eval(n.Right, sc)
}

func (e *Evaluator) evalIf(n *ast.If, sc *scope) object.Value {
	cond := e.eval(n.Cond, sc)
	if object.IsErr(cond) {
		return cond
	}
	if object.Truthy(cond) {
		return e.eval(n.Then, sc)
	}
	return e.eval(n.Else, sc)
}

// evalPipe feeds a value into a stage. An error only survives the pipe when the
// stage is a `match`, which exists precisely to handle it; every other stage
// propagates it, so a broken chain stops instead of running on bad data.
func (e *Evaluator) evalPipe(n *ast.Pipe, sc *scope) object.Value {
	left := e.eval(n.Left, sc)
	if m, isMatch := n.Stage.(*ast.Match); isMatch {
		return e.evalMatch(m, left, sc)
	}
	if object.IsErr(left) {
		return left
	}
	switch stage := n.Stage.(type) {
	case *ast.Transform:
		return e.evalTransform(stage, left, sc)
	case *ast.Verb:
		return e.evalVerb(stage, sc, left)
	}
	return e.fail(n.Stage, "%s cannot be used as a pipe stage", n.Stage.String())
}

// evalMatch dispatches on ok/err (spec §8.1). A subject with no matching arm is
// returned unchanged rather than swallowed — an unhandled error stays an error.
func (e *Evaluator) evalMatch(n *ast.Match, subject object.Value, sc *scope) object.Value {
	want := "ok"
	bound := subject
	failure, isFailure := subject.(*object.Err)
	if isFailure {
		want = "err"
		// The binder gets the reason text, which is what spec §8.1's
		// `"request failed: " + msg` concatenates. Spec §5.2 names the same
		// binder `code` and adds it to a string expecting an exit number; that
		// reads as a spec inconsistency rather than a second binding rule, so
		// the reason is bound in both places and carries the code inside it.
		bound = object.String(failure.Reason)
	}
	for _, arm := range n.Arms {
		if arm.Pattern != want {
			continue
		}
		if isFailure {
			failure.Handle()
		}
		armScope := sc.child()
		if arm.Binder != "_" {
			armScope.bind(arm.Binder, bound)
		}
		return e.evalStatement(arm.Body, armScope)
	}
	return subject
}
