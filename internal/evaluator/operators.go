package evaluator

import (
	"github.com/typedmirror/mana/internal/ast"
	"github.com/typedmirror/mana/internal/object"
)

// evalInfix applies a binary operator. `+` is the only overloaded one: it adds
// numbers and joins anything else as text, because spec §14 concatenates a
// string with a number without a cast and axiom 3 rules out asking the LLM to
// manage one.
func (e *Evaluator) evalInfix(n *ast.Infix, sc *scope) object.Value {
	left := e.eval(n.Left, sc)
	if object.IsErr(left) {
		return left
	}
	right := e.eval(n.Right, sc)
	if object.IsErr(right) {
		return right
	}

	switch n.Operator {
	case "is", "==":
		return object.Bool(equal(left, right))
	case "!=":
		return object.Bool(!equal(left, right))
	case "+":
		return e.add(n, left, right)
	case "-", "*", "/":
		return e.arithmetic(n, left, right)
	case "<", ">", "<=", ">=":
		return e.order(n, left, right)
	}
	return e.fail(n, "unknown operator %q", n.Operator)
}

func (e *Evaluator) add(n *ast.Infix, left, right object.Value) object.Value {
	ln, lok := left.(object.Number)
	rn, rok := right.(object.Number)
	if lok && rok {
		return ln + rn
	}
	switch left.(type) {
	case *object.List, *object.Record:
		return e.fail(n, "cannot add a %s to a %s", left.Type(), right.Type())
	}
	switch right.(type) {
	case *object.List, *object.Record:
		return e.fail(n, "cannot add a %s to a %s", left.Type(), right.Type())
	}
	return object.String(object.Text(left) + object.Text(right))
}

func (e *Evaluator) arithmetic(n *ast.Infix, left, right object.Value) object.Value {
	ln, lok := left.(object.Number)
	rn, rok := right.(object.Number)
	if !lok || !rok {
		return e.fail(n, "cannot apply %q to a %s and a %s", n.Operator, left.Type(), right.Type())
	}
	switch n.Operator {
	case "-":
		return ln - rn
	case "*":
		return ln * rn
	case "/":
		if rn == 0 {
			// Returning an error rather than +Inf: a report holding "+Inf" is a
			// silent failure wearing a number.
			return e.fail(n, "division by zero")
		}
		return ln / rn
	}
	return e.fail(n, "unknown operator %q", n.Operator)
}

// order compares with < and >. Numbers compare numerically and everything else
// compares as text, which is what makes spec §14's `last_login > "2025-01-01"`
// work on ISO dates without a date type.
func (e *Evaluator) order(n *ast.Infix, left, right object.Value) object.Value {
	switch left.(type) {
	case *object.List, *object.Record, object.Null:
		return e.fail(n, "cannot order a %s", left.Type())
	}
	switch right.(type) {
	case *object.List, *object.Record, object.Null:
		return e.fail(n, "cannot order a %s", right.Type())
	}
	c := object.Compare(left, right)
	switch n.Operator {
	case "<":
		return object.Bool(c < 0)
	case "<=":
		return object.Bool(c <= 0)
	case ">":
		return object.Bool(c > 0)
	}
	return object.Bool(c >= 0)
}

// equal is `is`. Numbers compare numerically, records and lists compare
// structurally, and a string equals a word with the same text — so `where role
// is "admin"` matches whether the field arrived quoted or bare.
func equal(a, b object.Value) bool {
	an, aok := a.(object.Number)
	bn, bok := b.(object.Number)
	if aok && bok {
		return an == bn
	}
	if aok != bok {
		return false
	}
	switch x := a.(type) {
	case object.Bool:
		y, ok := b.(object.Bool)
		return ok && x == y
	case *object.List:
		y, ok := b.(*object.List)
		if !ok || len(x.Elements) != len(y.Elements) {
			return false
		}
		for i := range x.Elements {
			if !equal(x.Elements[i], y.Elements[i]) {
				return false
			}
		}
		return true
	case *object.Record:
		y, ok := b.(*object.Record)
		if !ok || len(x.Keys()) != len(y.Keys()) {
			return false
		}
		for _, k := range x.Keys() {
			xv, _ := x.Get(k)
			yv, has := y.Get(k)
			if !has || !equal(xv, yv) {
				return false
			}
		}
		return true
	case object.Null:
		_, ok := b.(object.Null)
		return ok
	}
	if object.IsErr(a) || object.IsErr(b) {
		return false
	}
	return object.Text(a) == object.Text(b)
}
