// Package parser builds a Mana syntax tree from a token stream.
//
// It is a Pratt parser over a fully materialized token slice. Materializing is
// deliberate: axiom 2 says Mana scripts are ephemeral, so they are small, and
// unlimited lookahead makes the two genuinely ambiguous constructs — match arm
// boundaries and the optional arrow before a transform argument — resolvable
// without backtracking.
package parser

import (
	"fmt"
	"strconv"

	"github.com/typedmirror/mana/internal/ast"
	"github.com/typedmirror/mana/internal/lexer"
	"github.com/typedmirror/mana/internal/token"
)

// Precedence ladder. ARROW binds tighter than the arithmetic operators because
// spec §14 writes `@active -> count / @users -> count` and means
// `(@active -> count) / (@users -> count)`.
const (
	lowest int = iota
	fallback
	pipe
	compare
	sum
	product
	arrow
	prefix
	member
)

var precedences = map[token.Type]int{
	token.OR:       fallback,
	token.PIPE:     pipe,
	token.IS:       compare,
	token.EQ:       compare,
	token.NOTEQ:    compare,
	token.LT:       compare,
	token.GT:       compare,
	token.LTE:      compare,
	token.GTE:      compare,
	token.PLUS:     sum,
	token.MINUS:    sum,
	token.SLASH:    product,
	token.ASTERISK: product,
	token.ARROW:    arrow,
	token.DOT:      member,
}

// Parser turns tokens into a *ast.Program, accumulating every error it finds
// rather than stopping at the first.
type Parser struct {
	toks []token.Token
	i    int
	errs []string
}

// New returns a Parser over src.
func New(src string) *Parser {
	return &Parser{toks: lexer.Tokens(src)}
}

// Errors returns every parse error, in source order.
func (p *Parser) Errors() []string { return p.errs }

func (p *Parser) errorf(tok token.Token, format string, args ...any) {
	p.errs = append(p.errs, fmt.Sprintf("line %d: %s", tok.Line, fmt.Sprintf(format, args...)))
}

// --- cursor ------------------------------------------------------------------

func (p *Parser) cur() token.Token { return p.at(0) }

func (p *Parser) peek() token.Token { return p.at(1) }

func (p *Parser) at(n int) token.Token {
	i := p.i + n
	if i >= len(p.toks) {
		return p.toks[len(p.toks)-1] // EOF, which the lexer always emits last
	}
	return p.toks[i]
}

func (p *Parser) next() token.Token {
	tok := p.cur()
	if p.i < len(p.toks)-1 {
		p.i++
	}
	return tok
}

func (p *Parser) curIs(t token.Type) bool { return p.cur().Type == t }

// expect consumes a token of type t, or records an error and consumes nothing.
func (p *Parser) expect(t token.Type) bool {
	if p.curIs(t) {
		p.next()
		return true
	}
	p.errorf(p.cur(), "expected %s, got %s(%q)", t, p.cur().Type, p.cur().Literal)
	return false
}

func (p *Parser) skipNewlines() {
	for p.curIs(token.NEWLINE) {
		p.next()
	}
}

// --- program -----------------------------------------------------------------

// Parse consumes the whole token stream.
func (p *Parser) Parse() *ast.Program {
	prog := &ast.Program{}
	for {
		p.skipNewlines()
		if p.curIs(token.EOF) {
			return prog
		}
		before := p.i
		s := p.parseStatement()
		if s != nil {
			prog.Statements = append(prog.Statements, s)
		}
		// A statement that consumed nothing would spin forever; force progress
		// so one bad token yields one error instead of a hang.
		if p.i == before {
			p.next()
		}
		// An intent line ran to end of line in the lexer, so it carries its own
		// terminator and the next statement may start immediately.
		if _, intent := s.(*ast.IntentStatement); !intent {
			p.skipStatementEnd()
		}
	}
}

// skipStatementEnd consumes the terminator after a statement, complaining if
// what follows is neither a break nor a natural end.
func (p *Parser) skipStatementEnd() {
	switch p.cur().Type {
	case token.NEWLINE, token.EOF, token.RBRACE:
		return
	}
	p.errorf(p.cur(), "unexpected %s(%q) after statement", p.cur().Type, p.cur().Literal)
	for !p.curIs(token.NEWLINE) && !p.curIs(token.EOF) {
		p.next()
	}
}

func (p *Parser) parseStatement() ast.Statement {
	// `act` and `use` are contextual, not reserved. Axiom 1 says the syntax
	// should sit on words an LLM already emits — which is also a reason not to
	// take those words away from field names. They are recognised by shape:
	// `act "name"` and `use <module>`.
	if p.wordIs(0, "act") && p.at(1).Type == token.STRING {
		return p.parseAct()
	}
	if p.wordIs(0, "use") && isWord(p.at(1)) {
		tok := p.next()
		return &ast.Use{Tok: tok, Module: p.next().Literal}
	}

	switch p.cur().Type {
	case token.INTENT:
		tok := p.next()
		return &ast.IntentStatement{Tok: tok, Text: tok.Literal}
	case token.BINDING:
		// `@x = ...` binds; `@x |> ...` is an expression that happens to start
		// with a reference.
		if p.peek().Type == token.ASSIGN {
			tok := p.next()
			p.next() // '='
			return &ast.BindStatement{Tok: tok, Name: tok.Literal, Value: p.parseExpression(lowest)}
		}
	}
	tok := p.cur()
	// A bare word starting a statement, followed by something, is a module
	// verb. `postgres query "…"` cannot be anything else.
	if tok.Type == token.IDENT && p.startsModuleCall() {
		e := p.parseModuleCall()
		if e == nil {
			return nil
		}
		return &ast.ExpressionStatement{Tok: tok, Expression: e}
	}
	e := p.parseExpression(lowest)
	if e == nil {
		return nil
	}
	return &ast.ExpressionStatement{Tok: tok, Expression: e}
}

// startsModuleCall reports whether the current bare word is being used as a
// verb rather than as a value: something that can start an argument follows it.
//
// Clause keywords are deliberately not in that set, which is what keeps
// `fetch users from …` and `create user with …` from reading as module calls.
func (p *Parser) startsModuleCall() bool {
	return startsArgument(p.at(1).Type)
}

// --- expressions -------------------------------------------------------------

func (p *Parser) parseExpression(prec int) ast.Expression {
	left := p.parsePrefix(prec)
	if left == nil {
		return nil
	}
	for prec < precedences[p.cur().Type] {
		left = p.parseInfix(left)
		if left == nil {
			return nil
		}
	}
	return left
}

func (p *Parser) parsePrefix(prec int) ast.Expression {
	tok := p.cur()
	switch tok.Type {
	case token.IDENT:
		// A module call only where a whole expression is expected: a statement,
		// a binding, a record field, a list element. Clause values and verb
		// arguments parse tighter, and treating `sort by count descending` or
		// `create user with …` as a call would take a word that belongs to the
		// enclosing construct.
		if prec == lowest && p.startsModuleCall() {
			return p.parseModuleCall()
		}
		p.next()
		return &ast.Identifier{Tok: tok, Value: tok.Literal}
	case token.BINDING:
		p.next()
		return &ast.Binding{Tok: tok, Name: tok.Literal}
	case token.SELF:
		p.next()
		return &ast.Self{Tok: tok}
	case token.ACTREF:
		p.next()
		return &ast.ActRef{Tok: tok, Name: tok.Literal}
	case token.NUMBER:
		p.next()
		v, err := strconv.ParseFloat(tok.Literal, 64)
		if err != nil {
			p.errorf(tok, "could not parse %q as a number", tok.Literal)
			return nil
		}
		return &ast.NumberLiteral{Tok: tok, Value: v}
	case token.STRING, token.PATH, token.URL:
		p.next()
		return &ast.StringLiteral{Tok: tok, Value: tok.Literal, Kind: tok.Type}
	case token.TRUE, token.FALSE:
		p.next()
		return &ast.BooleanLiteral{Tok: tok, Value: tok.Type == token.TRUE}
	case token.MINUS:
		p.next()
		return &ast.Prefix{Tok: tok, Operator: "-", Right: p.parseExpression(prefix)}
	case token.LPAREN:
		p.next()
		e := p.parseExpression(lowest)
		p.expect(token.RPAREN)
		return e
	case token.LBRACKET:
		return p.parseList()
	case token.LBRACE:
		return p.parseRecord()
	case token.IF:
		return p.parseIf()
	case token.MATCH:
		return p.parseMatch()
	case token.ILLEGAL:
		p.next()
		p.errorf(tok, "unexpected character %q", tok.Literal)
		return nil
	}
	if token.IsVerb(tok.Type) {
		return p.parseVerb()
	}
	p.next()
	p.errorf(tok, "unexpected %s(%q)", tok.Type, tok.Literal)
	return nil
}

// parseModuleCall reads `NAME [target] ARG* CLAUSE*` (v2 §7.1).
//
// The first bare word after the module name is the target — always, even when
// an expression follows it. That is what separates `inventory check item "x"`
// (target `check`, clause `item`) from a clause list, and it is why the rule is
// positional rather than shape-based: `check` and `item` look identical.
func (p *Parser) parseModuleCall() ast.Expression {
	tok := p.next()
	call := &ast.ModuleCall{Tok: tok, Module: tok.Literal}

	if isWord(p.cur()) {
		call.Target = p.next().Literal
	}
	for {
		switch {
		case token.IsClause(p.cur().Type):
			c := p.parseOneClause()
			if c == nil {
				return nil
			}
			call.Clauses = append(call.Clauses, *c)
		case p.startsCustomClauseAt(0):
			kw := p.next()
			v := p.parseExpression(pipe)
			if v == nil {
				return nil
			}
			call.Clauses = append(call.Clauses, ast.Clause{Tok: kw, Kw: token.IDENT, Value: v})
		case startsArgument(p.cur().Type):
			a := p.parseExpression(pipe)
			if a == nil {
				return nil
			}
			call.Args = append(call.Args, a)
		default:
			return call
		}
	}
}

// startsCustomClauseAt reports whether a module-defined clause begins n tokens
// ahead: a bare word followed by something that can start an expression.
func (p *Parser) startsCustomClauseAt(n int) bool {
	tok := p.at(n)
	if tok.Type != token.IDENT {
		return false
	}
	return startsArgument(p.at(n + 1).Type)
}

func (p *Parser) parseInfix(left ast.Expression) ast.Expression {
	tok := p.cur()
	switch tok.Type {
	case token.OR:
		p.next()
		return &ast.Fallback{Tok: tok, Left: left, Right: p.parseExpression(fallback)}
	case token.PIPE, token.ARROW:
		p.next()
		stage := p.parseStage()
		if stage == nil {
			return nil
		}
		return &ast.Pipe{Tok: tok, Left: left, Stage: stage}
	case token.DOT:
		p.next()
		// Any word may follow a dot, keywords included: after a dot `to` is a
		// field name, not a clause.
		name := p.cur()
		if !isWord(name) {
			p.errorf(name, "expected a field name after '.', got %s(%q)", name.Type, name.Literal)
			return nil
		}
		p.next()
		return &ast.Member{Tok: tok, Object: left, Property: name.Literal}
	}
	prec := precedences[tok.Type]
	p.next()
	right := p.parseExpression(prec)
	if right == nil {
		return nil
	}
	return &ast.Infix{Tok: tok, Left: left, Operator: tok.Literal, Right: right}
}

func (p *Parser) parseList() ast.Expression {
	lit := &ast.ListLiteral{Tok: p.next()} // '['
	p.skipNewlines()
	for !p.curIs(token.RBRACKET) && !p.curIs(token.EOF) {
		e := p.parseExpression(lowest)
		if e == nil {
			return nil
		}
		lit.Elements = append(lit.Elements, e)
		if !p.skipSeparator(token.RBRACKET) {
			break
		}
	}
	p.expect(token.RBRACKET)
	return lit
}

func (p *Parser) parseRecord() ast.Expression {
	lit := &ast.RecordLiteral{Tok: p.next()} // '{'
	p.skipNewlines()
	for !p.curIs(token.RBRACE) && !p.curIs(token.EOF) {
		key := p.cur()
		if !isWord(key) {
			p.errorf(key, "expected a field name in a record, got %s(%q)", key.Type, key.Literal)
			return nil
		}
		p.next()
		if !p.expect(token.COLON) {
			return nil
		}
		p.skipNewlines()
		v := p.parseExpression(lowest)
		if v == nil {
			return nil
		}
		lit.Pairs = append(lit.Pairs, ast.Pair{Key: key.Literal, Value: v})
		if !p.skipSeparator(token.RBRACE) {
			break
		}
	}
	p.expect(token.RBRACE)
	return lit
}

// skipSeparator consumes the comma or line break between elements. Spec §9
// writes records both ways — commas on one line, breaks across several — so
// both are accepted and neither is required before the closer.
func (p *Parser) skipSeparator(closer token.Type) bool {
	switch p.cur().Type {
	case token.COMMA:
		p.next()
		p.skipNewlines()
		return true
	case token.NEWLINE:
		p.skipNewlines()
		return true
	case closer:
		return false
	}
	p.errorf(p.cur(), "expected ',' or a line break, got %s(%q)", p.cur().Type, p.cur().Literal)
	return false
}

func (p *Parser) parseIf() ast.Expression {
	e := &ast.If{Tok: p.next()} // 'if'
	e.Cond = p.parseExpression(lowest)
	if !p.expect(token.THEN) {
		return nil
	}
	e.Then = p.parseExpression(lowest)
	if !p.expect(token.ELSE) {
		return nil
	}
	e.Else = p.parseExpression(lowest)
	if e.Cond == nil || e.Then == nil || e.Else == nil {
		return nil
	}
	return e
}

// --- operations --------------------------------------------------------------

// parseStage parses the right-hand side of `|>` or `->`. A stage is a verb, a
// match, or a named transform — never a bare identifier, because in stage
// position `count` means "apply count", not "the value of count".
func (p *Parser) parseStage() ast.Expression {
	switch {
	case p.curIs(token.MATCH):
		return p.parseMatch()
	case token.IsVerb(p.cur().Type):
		return p.parseVerb()
	case p.curIs(token.IDENT):
		if p.startsCustomClauseAt(0) || isWord(p.at(1)) {
			// `… |> slack send channel "ops"` — two words in stage position is
			// a module call, not a transform with an argument.
			if _, builtin := builtinTransforms[p.cur().Literal]; !builtin {
				return p.parseModuleCall()
			}
		}
		return p.parseTransform()
	}
	// Nothing else can act on a piped value. Rejecting it here rather than at
	// runtime means `@x |> 5` is a syntax error, which is what it is.
	tok := p.cur()
	p.next()
	p.errorf(tok, "%s(%q) cannot be a pipe stage — expected a verb, a transform, or match", tok.Type, tok.Literal)
	return nil
}

func (p *Parser) parseTransform() ast.Expression {
	tok := p.next()
	t := &ast.Transform{Tok: tok, Name: tok.Literal}

	// `map -> { ... }` (spec §7.1) and `map { ... }` (§7.3) mean the same thing.
	// The arrow is consumed as sugar only when what follows cannot be another
	// transform, which keeps `trim -> lowercase` a two-stage chain.
	if p.curIs(token.ARROW) && startsArgument(p.peek().Type) && p.peek().Type != token.IDENT {
		p.next()
	}
	// The argument binds at `arrow`, not `pipe`, so it stops before a following
	// `->`. Parsed any looser, `map name -> trim` would read as
	// `map (name -> trim)` and the rest of the chain would vanish into the
	// argument — a whole stage silently absorbed.
	if startsArgument(p.cur().Type) && !isDirection(p.cur()) {
		t.Arg = p.parseExpression(arrow)
	}
	t.Clauses = p.parseClauses()
	// `sort by count descending` — the direction trails the clause it modifies.
	if isDirection(p.cur()) {
		t.Direction = p.next().Literal
	}
	return t
}

// isDirection reports whether tok is an ordering direction. Full words only:
// `desc` would be an abbreviation, and Mana's syntax is meant to sit on words
// an LLM already emits (axiom 1).
func isDirection(tok token.Token) bool {
	return tok.Type == token.IDENT && (tok.Literal == "ascending" || tok.Literal == "descending")
}

func (p *Parser) parseVerb() ast.Expression {
	tok := p.next()
	v := &ast.Verb{Tok: tok, Verb: tok.Type}

	// `run` carries a raw shell line rather than parsed arguments (spec §5.2).
	if tok.Type == token.RUN && p.curIs(token.RAW) {
		v.Shell = p.next().Literal
		v.Clauses = p.parseClauses()
		return v
	}
	// `run tool <name>` and `send err <reason>` are language forms whose first
	// argument is a bare word followed by a value — the same shape as a module
	// call. Consumed here so generic parsing never sees them.
	for _, form := range []struct {
		verb token.Type
		word string
	}{{token.RUN, "tool"}, {token.SEND, "err"}} {
		if tok.Type == form.verb && p.wordIs(0, form.word) {
			marker := p.next()
			v.Args = append(v.Args, &ast.Identifier{Tok: marker, Value: marker.Literal})
			if form.word == "tool" && isWord(p.cur()) {
				name := p.next()
				v.Args = append(v.Args, &ast.Identifier{Tok: name, Value: name.Literal})
			}
			break
		}
	}
	// Arguments stop at `|>`. Two alternatives were tried and measured, and
	// both were worse:
	//
	//   - Lowering every verb's argument below `|>` fixes `send @x |> f` but
	//     breaks `fetch <url> |> count`, which then pipes the URL into count.
	//   - Lowering it for `send` and `ask` only — the verbs whose argument is a
	//     value rather than a locator — works until a trailing clause, where
	//     `send @x |> f to output` absorbs `to output` as a clause of `f` and
	//     silently sets the act result instead of printing. Trading a loud trap
	//     for a silent one is not a trade.
	//
	// So the shape stays, and evalPipe carries a diagnostic naming the fix.
	for startsArgument(p.cur().Type) {
		a := p.parseExpression(pipe)
		if a == nil {
			return nil
		}
		v.Args = append(v.Args, a)
	}
	v.Clauses = p.parseClauses()
	if tok.Type == token.RUN && v.Shell == "" && len(v.Args) == 0 {
		p.errorf(tok, "run needs a command")
		return nil
	}
	return v
}

func (p *Parser) parseClauses() []ast.Clause {
	var out []ast.Clause
	sawTo := false
	for {
		switch {
		case token.IsClause(p.cur().Type):
			if p.curIs(token.TO) {
				sawTo = true
			}
			c := p.parseOneClause()
			if c == nil {
				return out
			}
			out = append(out, *c)
		case sawTo && p.startsCustomClauseAt(0):
			// `send @x to slack channel "ops"` — once a destination names a
			// module, what follows can be that module's own clause. Allowed
			// only after `to`, because a bare word before any clause is an
			// argument: `run tool search_web …` must keep both words.
			kw := p.next()
			v := p.parseExpression(pipe)
			if v == nil {
				return out
			}
			out = append(out, ast.Clause{Tok: kw, Kw: token.IDENT, Value: v})
		default:
			return out
		}
	}
}

func (p *Parser) parseOneClause() *ast.Clause {
	tok := p.next()
	v := p.parseClauseValue(tok.Type)
	if v == nil {
		return nil
	}
	return &ast.Clause{Tok: tok, Kw: tok.Type, Value: v}
}

func (p *Parser) parseClauseValue(kw token.Type) ast.Expression {
	// `with name "alice"` (spec §5.3) is a one-field record written without
	// braces. It is only that when a bare name is followed by a value; `with
	// @content` and `with { ... }` are ordinary expressions.
	if kw == token.WITH && p.startsCustomClauseAt(0) {
		rec := &ast.RecordLiteral{Tok: p.cur()}
		for p.startsCustomClauseAt(0) {
			key := p.next()
			v := p.parseExpression(pipe)
			if v == nil {
				return nil
			}
			rec.Pairs = append(rec.Pairs, ast.Pair{Key: key.Literal, Value: v})
		}
		return rec
	}
	return p.parseExpression(pipe)
}

// parseAct reads an act declaration (spec v2 §4).
//
//	act "name" { … }
//	act "name" depends on "a", "b" { … }
//	act "name" from ./other.mana
func (p *Parser) parseAct() ast.Statement {
	tok := p.next() // 'act'
	a := &ast.Act{Tok: tok, Name: p.next().Literal}
	if a.Name == "" {
		p.errorf(tok, "an act needs a name")
		return nil
	}

	if p.wordIs(0, "from") || p.curIs(token.FROM) {
		p.next()
		src := p.cur()
		if src.Type != token.PATH && src.Type != token.STRING {
			p.errorf(src, "act %q: `from` needs a path, got %s(%q)", a.Name, src.Type, src.Literal)
			return nil
		}
		p.next()
		a.From = src.Literal
		return a
	}

	if p.wordIs(0, "depends") {
		p.next()
		if !p.wordIs(0, "on") {
			p.errorf(p.cur(), "act %q: expected `on` after `depends`", a.Name)
			return nil
		}
		p.next()
		for {
			dep := p.cur()
			if dep.Type != token.STRING {
				p.errorf(dep, "act %q: a dependency is a quoted act name, got %s(%q)", a.Name, dep.Type, dep.Literal)
				return nil
			}
			p.next()
			a.Depends = append(a.Depends, dep.Literal)
			if !p.curIs(token.COMMA) {
				break
			}
			p.next()
			p.skipNewlines()
		}
	}

	if !p.expect(token.LBRACE) {
		return nil
	}
	body := &ast.Block{Tok: p.cur()}
	for {
		p.skipNewlines()
		if p.curIs(token.RBRACE) || p.curIs(token.EOF) {
			break
		}
		before := p.i
		s := p.parseStatement()
		switch inner := s.(type) {
		case *ast.Use:
			// Collected onto the act rather than left in the body: the use set
			// is the act's permission boundary, not a runtime step.
			a.Uses = append(a.Uses, inner.Module)
		case *ast.Act:
			p.errorf(inner.Tok, "act %q: acts cannot be nested", a.Name)
		case nil:
		default:
			body.Statements = append(body.Statements, s)
		}
		if p.i == before {
			p.next()
		}
		if _, intent := s.(*ast.IntentStatement); !intent && !p.curIs(token.RBRACE) {
			p.skipStatementEnd()
		}
	}
	if !p.expect(token.RBRACE) {
		return nil
	}
	a.Body = body
	return a
}

// wordIs reports whether the token n ahead is the bare word w.
func (p *Parser) wordIs(n int, w string) bool {
	tok := p.at(n)
	return isWord(tok) && tok.Literal == w
}

// isWord reports whether tok is a bare word — an identifier or a keyword being
// used as one.
func isWord(tok token.Token) bool {
	if tok.Type == token.IDENT {
		return true
	}
	return tok.Literal != "" && token.LookupIdent(tok.Literal) == tok.Type
}

// startsArgument reports whether t can begin a verb argument or transform
// argument. Infix operators are excluded so `count / @users` reads as division
// rather than as `count` applied to a path.
func startsArgument(t token.Type) bool {
	switch t {
	case token.IDENT, token.BINDING, token.SELF, token.NUMBER, token.STRING,
		token.PATH, token.URL, token.TRUE, token.FALSE, token.ACTREF,
		token.LBRACE, token.LBRACKET, token.LPAREN, token.IF, token.MATCH:
		return true
	}
	return token.IsVerb(t)
}

// --- match -------------------------------------------------------------------

func (p *Parser) parseMatch() ast.Expression {
	m := &ast.Match{Tok: p.next()} // 'match'
	if !p.expect(token.LBRACE) {
		return nil
	}
	p.skipNewlines()
	for !p.curIs(token.RBRACE) && !p.curIs(token.EOF) {
		arm, ok := p.parseArm()
		if !ok {
			return nil
		}
		m.Arms = append(m.Arms, arm)
		p.skipNewlines()
	}
	if !p.expect(token.RBRACE) {
		return nil
	}
	if len(m.Arms) == 0 {
		p.errorf(m.Tok, "match needs at least one arm")
		return nil
	}
	return m
}

func (p *Parser) parseArm() (ast.MatchArm, bool) {
	tok := p.cur()
	if !p.armStartsAt(0) {
		p.errorf(tok, "expected a match arm like `ok value:` or `err msg:`, got %s(%q)", tok.Type, tok.Literal)
		return ast.MatchArm{}, false
	}
	arm := ast.MatchArm{Tok: tok, Pattern: p.next().Literal, Binder: p.next().Literal}
	p.next() // ':'

	body := &ast.Block{Tok: p.cur()}
	for {
		p.skipNewlines()
		if p.curIs(token.RBRACE) || p.curIs(token.EOF) || p.armStartsAt(0) {
			break
		}
		before := p.i
		if s := p.parseStatement(); s != nil {
			body.Statements = append(body.Statements, s)
		}
		if p.i == before {
			p.next()
		}
	}
	if len(body.Statements) == 0 {
		p.errorf(arm.Tok, "match arm %q has an empty body", arm.Pattern)
		return ast.MatchArm{}, false
	}
	arm.Body = body
	return arm, true
}

// armStartsAt reports whether an arm header — `ok x:` or `err x:` — begins n
// tokens ahead. This is the lookahead that lets an arm body run to the next arm
// without a terminator of its own.
func (p *Parser) armStartsAt(n int) bool {
	head := p.at(n)
	if head.Type != token.IDENT || (head.Literal != "ok" && head.Literal != "err") {
		return false
	}
	return p.at(n+1).Type == token.IDENT && p.at(n+2).Type == token.COLON
}

// builtinTransforms are the names resolved as data operations rather than as
// module verbs. The parser needs the set only to break the tie in stage
// position, where `map name` and `postgres query` have the same shape.
var builtinTransforms = map[string]struct{}{
	"filter": {}, "map": {}, "sort": {}, "group": {}, "take": {},
	"count": {}, "sum": {}, "trim": {}, "lowercase": {}, "matches": {},
}
