// Package object defines Mana's runtime values.
//
// The load-bearing decision here is spec §10: errors are data. There is no
// exception type and no panic path. Every verb returns a Value, and a failure
// is an *Err — an ordinary value that `or` can catch, `match` can dispatch on,
// and the runtime can print with the reasoning that preceded it.
package object

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Value is any Mana runtime value.
type Value interface {
	Type() string
	Inspect() string
}

// --- scalars -----------------------------------------------------------------

type Number float64

func (v Number) Type() string { return "number" }
func (v Number) Inspect() string {
	return strconv.FormatFloat(float64(v), 'f', -1, 64)
}

type String string

func (v String) Type() string    { return "string" }
func (v String) Inspect() string { return string(v) }

type Bool bool

func (v Bool) Type() string    { return "bool" }
func (v Bool) Inspect() string { return strconv.FormatBool(bool(v)) }

// Word is a bare identifier that names something rather than holding a value:
// the `output` in `send @x to output`, the `csv` in `as csv`. It is deliberately
// distinct from String so a destination cannot be confused with data.
type Word string

func (v Word) Type() string    { return "word" }
func (v Word) Inspect() string { return string(v) }

// Null is the absence of a value. Verbs that succeed without producing data —
// `write`, `send` — return it rather than a fake success payload.
type Null struct{}

func (Null) Type() string    { return "null" }
func (Null) Inspect() string { return "null" }

// --- collections -------------------------------------------------------------

type List struct {
	Elements []Value
}

func (v *List) Type() string { return "list" }
func (v *List) Inspect() string {
	parts := make([]string, len(v.Elements))
	for i, e := range v.Elements {
		parts[i] = e.Inspect()
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// Record keeps insertion order. Output that reorders itself between runs is
// output nobody can diff, and a report is meant to be read.
type Record struct {
	keys []string
	vals map[string]Value
}

func NewRecord() *Record { return &Record{vals: map[string]Value{}} }

func (v *Record) Type() string { return "record" }

func (v *Record) Keys() []string { return v.keys }

func (v *Record) Get(k string) (Value, bool) {
	x, ok := v.vals[k]
	return x, ok
}

func (v *Record) Set(k string, val Value) {
	if _, seen := v.vals[k]; !seen {
		v.keys = append(v.keys, k)
	}
	v.vals[k] = val
}

func (v *Record) Inspect() string {
	parts := make([]string, 0, len(v.keys))
	for _, k := range v.keys {
		parts = append(parts, k+": "+v.vals[k].Inspect())
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

// --- errors ------------------------------------------------------------------

// Err is a failed result. The Intent field is what makes Mana's error model
// different from a stack trace: it carries the agent's own reasoning from the
// `--` line that preceded the failure (spec §10.1, §15.3).
type Err struct {
	At         string
	Intent     string
	Reason     string
	Suggestion string
	Line       int

	// handled records that `or` or a `match` err arm dealt with this failure.
	// Without it a script could bind a failure, never look at it, and exit
	// clean — which is the silent failure the language exists to prevent.
	handled bool
}

func (v *Err) Type() string { return "err" }

// Handle marks the failure as dealt with.
func (v *Err) Handle() { v.handled = true }

// Handled reports whether anything took responsibility for this failure.
func (v *Err) Handled() bool { return v.handled }

// Inspect renders the error value in the shape spec §10.1 defines. Every field
// the spec names is present; `intent` appears even when empty, because a blank
// intent means the failure had no reasoning in front of it and that is worth
// seeing rather than hiding.
func (v *Err) Inspect() string {
	var b strings.Builder
	b.WriteString("{ status: err\n")
	fmt.Fprintf(&b, "  at: %q\n", v.At)
	fmt.Fprintf(&b, "  intent: %q\n", v.Intent)
	fmt.Fprintf(&b, "  reason: %q", v.Reason)
	if v.Suggestion != "" {
		fmt.Fprintf(&b, "\n  suggestion: %q", v.Suggestion)
	}
	b.WriteString(" }")
	return b.String()
}

// IsErr reports whether v is a failure. Callers use it instead of a type switch
// so the check reads the same everywhere it appears.
func IsErr(v Value) bool {
	_, bad := v.(*Err)
	return bad
}

// Errorf builds an *Err. At and Intent are filled in by the evaluator, which is
// the only layer that knows both the operation and the intent stack.
func Errorf(format string, args ...any) *Err {
	return &Err{Reason: fmt.Sprintf(format, args...)}
}

// --- helpers -----------------------------------------------------------------

// Truthy defines what counts as true in a condition. Empty collections and
// empty strings are false, because `filter where tags` should mean "has tags".
func Truthy(v Value) bool {
	switch x := v.(type) {
	case Bool:
		return bool(x)
	case Null:
		return false
	case Number:
		return x != 0
	case String:
		return x != ""
	case Word:
		return x != ""
	case *List:
		return len(x.Elements) > 0
	case *Record:
		return len(x.keys) > 0
	case *Err:
		return false
	}
	return true
}

// Text renders a value for concatenation and for prompts. It differs from
// Inspect only for strings, where quoting would be noise.
func Text(v Value) string { return v.Inspect() }

// JSON renders a value as JSON, used by `write ... as json` and by `send` to an
// HTTP destination.
func JSON(v Value) string {
	switch x := v.(type) {
	case Number:
		return strconv.FormatFloat(float64(x), 'f', -1, 64)
	case Bool:
		return strconv.FormatBool(bool(x))
	case String:
		return strconv.Quote(string(x))
	case Word:
		return strconv.Quote(string(x))
	case Null:
		return "null"
	case *List:
		parts := make([]string, len(x.Elements))
		for i, e := range x.Elements {
			parts[i] = JSON(e)
		}
		return "[" + strings.Join(parts, ",") + "]"
	case *Record:
		parts := make([]string, 0, len(x.keys))
		for _, k := range x.keys {
			parts = append(parts, strconv.Quote(k)+":"+JSON(x.vals[k]))
		}
		return "{" + strings.Join(parts, ",") + "}"
	case *Err:
		r := NewRecord()
		r.Set("status", String("err"))
		r.Set("at", String(x.At))
		r.Set("intent", String(x.Intent))
		r.Set("reason", String(x.Reason))
		return JSON(r)
	}
	return "null"
}

// CSV renders a list of records as CSV, for `write ... as csv`. Columns are the
// union of every record's keys, in first-seen order, so a row missing a field
// yields an empty cell instead of a shifted table.
func CSV(v Value) (string, *Err) {
	list, ok := v.(*List)
	if !ok {
		return "", Errorf("csv needs a list of records, got %s", v.Type())
	}
	var cols []string
	seen := map[string]bool{}
	for _, e := range list.Elements {
		r, ok := e.(*Record)
		if !ok {
			return "", Errorf("csv needs a list of records, found a %s in the list", e.Type())
		}
		for _, k := range r.keys {
			if !seen[k] {
				seen[k] = true
				cols = append(cols, k)
			}
		}
	}
	var b strings.Builder
	b.WriteString(strings.Join(cols, ","))
	for _, e := range list.Elements {
		r := e.(*Record)
		cells := make([]string, len(cols))
		for i, c := range cols {
			if val, ok := r.Get(c); ok {
				cells[i] = csvCell(Text(val))
			}
		}
		b.WriteString("\n" + strings.Join(cells, ","))
	}
	return b.String(), nil
}

func csvCell(s string) string {
	if strings.ContainsAny(s, ",\"\n") {
		return strconv.Quote(s)
	}
	return s
}

// Compare orders two values for `sort`. Numbers compare numerically, everything
// else by rendered text, so a mixed list still has a stable total order.
func Compare(a, b Value) int {
	an, aok := a.(Number)
	bn, bok := b.(Number)
	if aok && bok {
		switch {
		case an < bn:
			return -1
		case an > bn:
			return 1
		}
		return 0
	}
	return strings.Compare(Text(a), Text(b))
}

// SortBy sorts elements in place with a stable sort, so equal keys keep their
// input order.
func SortBy(elems []Value, key func(Value) Value) {
	sort.SliceStable(elems, func(i, j int) bool {
		return Compare(key(elems[i]), key(elems[j])) < 0
	})
}
