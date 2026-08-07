// Package ast defines the syntax tree Mana parses into.
//
// The tree is dual-channel like the source (spec §3): IntentStatement nodes sit
// in the statement list alongside executable ones, in source order. They are not
// stripped, because the evaluator needs the reasoning that preceded a failure in
// order to report it (spec §15.3).
package ast

import (
	"strconv"
	"strings"

	"github.com/typedmirror/mana/internal/token"
)

// Node is any tree node. Line reports where it started, so a runtime error can
// point at source.
type Node interface {
	Line() int
	String() string
}

type Statement interface {
	Node
	statementNode()
}

type Expression interface {
	Node
	expressionNode()
}

// Program is a whole script: a flat sequence of statements from both channels.
type Program struct {
	Statements []Statement
}

func (p *Program) Line() int {
	if len(p.Statements) == 0 {
		return 0
	}
	return p.Statements[0].Line()
}

func (p *Program) String() string {
	var b strings.Builder
	for i, s := range p.Statements {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(s.String())
	}
	return b.String()
}

// --- statements --------------------------------------------------------------

// IntentStatement is a `--` line. Runtime metadata, not a comment.
type IntentStatement struct {
	Tok  token.Token
	Text string
}

func (s *IntentStatement) statementNode() {}
func (s *IntentStatement) Line() int      { return s.Tok.Line }
func (s *IntentStatement) String() string { return "-- " + s.Text }

// BindStatement is `@name = expr` (spec §4.1). One sigil, one job.
type BindStatement struct {
	Tok   token.Token
	Name  string
	Value Expression
}

func (s *BindStatement) statementNode() {}
func (s *BindStatement) Line() int      { return s.Tok.Line }
func (s *BindStatement) String() string { return "@" + s.Name + " = " + s.Value.String() }

// ExpressionStatement is a bare operation evaluated for its effect.
type ExpressionStatement struct {
	Tok        token.Token
	Expression Expression
}

func (s *ExpressionStatement) statementNode() {}
func (s *ExpressionStatement) Line() int      { return s.Tok.Line }
func (s *ExpressionStatement) String() string { return s.Expression.String() }

// Act is Mana's execution unit (spec v2 §4). It owns an intent stack, a tool
// scope, and a result. Acts with no dependency edge between them run
// concurrently — the dependency graph is the concurrency model.
//
// From is set instead of Body when the act is imported from another file
// (§4.5).
type Act struct {
	Tok     token.Token
	Name    string
	Depends []string
	Uses    []string
	Body    *Block
	From    string
}

func (s *Act) statementNode() {}
func (s *Act) Line() int      { return s.Tok.Line }
func (s *Act) String() string {
	var b strings.Builder
	b.WriteString("act " + strconv.Quote(s.Name))
	if s.From != "" {
		return b.String() + " from " + s.From
	}
	if len(s.Depends) > 0 {
		quoted := make([]string, len(s.Depends))
		for i, d := range s.Depends {
			quoted[i] = strconv.Quote(d)
		}
		b.WriteString(" depends on " + strings.Join(quoted, ", "))
	}
	b.WriteString(" {")
	for _, u := range s.Uses {
		b.WriteString(" use " + u + ";")
	}
	if s.Body != nil && len(s.Body.Statements) > 0 {
		b.WriteString(" " + s.Body.String())
	}
	b.WriteString(" }")
	return b.String()
}

// Use loads a module into the enclosing act's scope (spec v2 §7.1). The `use`
// boundary is the permission boundary — an act that does not use a module
// cannot reach it.
type Use struct {
	Tok    token.Token
	Module string
}

func (s *Use) statementNode() {}
func (s *Use) Line() int      { return s.Tok.Line }
func (s *Use) String() string { return "use " + s.Module }

// ActRef is `act.<name>.result` (spec v2 §4.3).
type ActRef struct {
	Tok  token.Token
	Name string
}

func (e *ActRef) expressionNode() {}
func (e *ActRef) Line() int       { return e.Tok.Line }
func (e *ActRef) String() string  { return "act." + e.Name + ".result" }

// Block is a sequence of statements, used for match arm bodies (spec §8.1).
type Block struct {
	Tok        token.Token
	Statements []Statement
}

func (b *Block) statementNode() {}
func (b *Block) Line() int      { return b.Tok.Line }
func (b *Block) String() string {
	parts := make([]string, len(b.Statements))
	for i, s := range b.Statements {
		parts[i] = s.String()
	}
	return strings.Join(parts, "; ")
}

// --- atoms -------------------------------------------------------------------

// Identifier is a bare name: a field, a transform, a destination, `context`.
type Identifier struct {
	Tok   token.Token
	Value string
}

func (e *Identifier) expressionNode() {}
func (e *Identifier) Line() int       { return e.Tok.Line }
func (e *Identifier) String() string  { return e.Value }

// Binding is `@name` — a reference to a bound value.
type Binding struct {
	Tok  token.Token
	Name string
}

func (e *Binding) expressionNode() {}
func (e *Binding) Line() int       { return e.Tok.Line }
func (e *Binding) String() string  { return "@" + e.Name }

// Self is `@` inside a transform: the element currently being operated on
// (spec §7.3).
type Self struct {
	Tok token.Token
}

func (e *Self) expressionNode() {}
func (e *Self) Line() int       { return e.Tok.Line }
func (e *Self) String() string  { return "@" }

type NumberLiteral struct {
	Tok   token.Token
	Value float64
}

func (e *NumberLiteral) expressionNode() {}
func (e *NumberLiteral) Line() int       { return e.Tok.Line }
func (e *NumberLiteral) String() string  { return strconv.FormatFloat(e.Value, 'g', -1, 64) }

// StringLiteral covers quoted strings and the bare paths and URLs the spec
// writes unquoted. Kind records which, so errors can name it.
type StringLiteral struct {
	Tok   token.Token
	Value string
	Kind  token.Type
}

func (e *StringLiteral) expressionNode() {}
func (e *StringLiteral) Line() int       { return e.Tok.Line }
func (e *StringLiteral) String() string {
	if e.Kind == token.STRING {
		return strconv.Quote(e.Value)
	}
	return e.Value
}

type BooleanLiteral struct {
	Tok   token.Token
	Value bool
}

func (e *BooleanLiteral) expressionNode() {}
func (e *BooleanLiteral) Line() int       { return e.Tok.Line }
func (e *BooleanLiteral) String() string  { return strconv.FormatBool(e.Value) }

type ListLiteral struct {
	Tok      token.Token
	Elements []Expression
}

func (e *ListLiteral) expressionNode() {}
func (e *ListLiteral) Line() int       { return e.Tok.Line }
func (e *ListLiteral) String() string {
	parts := make([]string, len(e.Elements))
	for i, el := range e.Elements {
		parts[i] = el.String()
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// Pair is one field of a record literal.
type Pair struct {
	Key   string
	Value Expression
}

type RecordLiteral struct {
	Tok   token.Token
	Pairs []Pair
}

func (e *RecordLiteral) expressionNode() {}
func (e *RecordLiteral) Line() int       { return e.Tok.Line }
func (e *RecordLiteral) String() string {
	parts := make([]string, len(e.Pairs))
	for i, p := range e.Pairs {
		parts[i] = p.Key + ": " + p.Value.String()
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

// --- operators ---------------------------------------------------------------

type Prefix struct {
	Tok      token.Token
	Operator string
	Right    Expression
}

func (e *Prefix) expressionNode() {}
func (e *Prefix) Line() int       { return e.Tok.Line }
func (e *Prefix) String() string  { return "(" + e.Operator + e.Right.String() + ")" }

type Infix struct {
	Tok      token.Token
	Left     Expression
	Operator string
	Right    Expression
}

func (e *Infix) expressionNode() {}
func (e *Infix) Line() int       { return e.Tok.Line }
func (e *Infix) String() string {
	return "(" + e.Left.String() + " " + e.Operator + " " + e.Right.String() + ")"
}

// Member is field access: `@user.name`, `context.env.cwd`, `@.name`.
type Member struct {
	Tok      token.Token
	Object   Expression
	Property string
}

func (e *Member) expressionNode() {}
func (e *Member) Line() int       { return e.Tok.Line }
func (e *Member) String() string  { return e.Object.String() + "." + e.Property }

// Fallback is `left or right` (spec §8.3). Distinct from a boolean or: it
// dispatches on whether the left side produced an error, not on truthiness.
type Fallback struct {
	Tok   token.Token
	Left  Expression
	Right Expression
}

func (e *Fallback) expressionNode() {}
func (e *Fallback) Line() int       { return e.Tok.Line }
func (e *Fallback) String() string  { return "(" + e.Left.String() + " or " + e.Right.String() + ")" }

// Pipe feeds the left value into a stage (spec §7.1). `|>` and `->` build the
// same node; they differ only in how tightly they bind.
type Pipe struct {
	Tok   token.Token
	Left  Expression
	Stage Expression
}

func (e *Pipe) expressionNode() {}
func (e *Pipe) Line() int       { return e.Tok.Line }
func (e *Pipe) String() string {
	return "(" + e.Left.String() + " " + e.Tok.Literal + " " + e.Stage.String() + ")"
}

type If struct {
	Tok  token.Token
	Cond Expression
	Then Expression
	Else Expression
}

func (e *If) expressionNode() {}
func (e *If) Line() int       { return e.Tok.Line }
func (e *If) String() string {
	return "if " + e.Cond.String() + " then " + e.Then.String() + " else " + e.Else.String()
}

// --- operations --------------------------------------------------------------

// Clause is a keyword-value modifier on a verb or transform (spec §6).
// Resolution is by keyword, never by position.
type Clause struct {
	Tok   token.Token
	Kw    token.Type
	Value Expression
}

func clauseList(cs []Clause) string {
	var b strings.Builder
	for _, c := range cs {
		b.WriteString(" " + c.Tok.Literal + " " + c.Value.String())
	}
	return b.String()
}

// Verb is one of the seven execution primitives with its arguments and clauses
// (spec §5). Shell holds the raw command line when the verb is `run`.
type Verb struct {
	Tok     token.Token
	Verb    token.Type
	Args    []Expression
	Clauses []Clause
	Shell   string
}

func (e *Verb) expressionNode() {}
func (e *Verb) Line() int       { return e.Tok.Line }
func (e *Verb) String() string {
	var b strings.Builder
	b.WriteString(e.Tok.Literal)
	if e.Shell != "" {
		b.WriteString(" " + e.Shell)
	}
	for _, a := range e.Args {
		b.WriteString(" " + a.String())
	}
	b.WriteString(clauseList(e.Clauses))
	return b.String()
}

// Clause finds a clause by keyword. The second result reports presence, so an
// absent clause is distinguishable from one holding a zero value.
func (e *Verb) Clause(kw token.Type) (Expression, bool) {
	for _, c := range e.Clauses {
		if c.Kw == kw {
			return c.Value, true
		}
	}
	return nil, false
}

// Transform is a named data operation in a pipe stage: `filter where active`,
// `map name`, `sort by name`, `take 5` (spec §7).
//
// Direction is "ascending", "descending", or "" when unstated. Full words, not
// abbreviations: the syntax sits on tokens an LLM already emits (axiom 1).
type Transform struct {
	Tok       token.Token
	Name      string
	Arg       Expression
	Clauses   []Clause
	Direction string
}

func (e *Transform) expressionNode() {}
func (e *Transform) Line() int       { return e.Tok.Line }
func (e *Transform) String() string {
	var b strings.Builder
	b.WriteString(e.Name)
	if e.Arg != nil {
		b.WriteString(" " + e.Arg.String())
	}
	b.WriteString(clauseList(e.Clauses))
	if e.Direction != "" {
		b.WriteString(" " + e.Direction)
	}
	return b.String()
}

func (e *Transform) Clause(kw token.Type) (Expression, bool) {
	for _, c := range e.Clauses {
		if c.Kw == kw {
			return c.Value, true
		}
	}
	return nil, false
}

// MatchArm is one `pattern binder: body` case. Binder is "_" when the value is
// discarded.
type MatchArm struct {
	Tok     token.Token
	Pattern string // "ok" or "err"
	Binder  string
	Body    *Block
}

// Match dispatches on whether a result is ok or err (spec §8.1).
type Match struct {
	Tok     token.Token
	Subject Expression // nil when the subject arrives through a pipe
	Arms    []MatchArm
}

func (e *Match) expressionNode() {}
func (e *Match) Line() int       { return e.Tok.Line }
func (e *Match) String() string {
	var b strings.Builder
	b.WriteString("match {")
	for _, a := range e.Arms {
		b.WriteString(" " + a.Pattern + " " + a.Binder + ": " + a.Body.String())
	}
	b.WriteString(" }")
	return b.String()
}
