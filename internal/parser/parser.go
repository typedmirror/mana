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
	e := p.parseExpression(lowest)
	if e == nil {
		return nil
	}
	return &ast.ExpressionStatement{Tok: tok, Expression: e}
}

// --- expressions -------------------------------------------------------------

func (p *Parser) parseExpression(prec int) ast.Expression {
	left := p.parsePrefix()
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

func (p *Parser) parsePrefix() ast.Expression {
	tok := p.cur()
	switch tok.Type {
	case token.IDENT:
		p.next()
		return &ast.Identifier{Tok: tok, Value: tok.Literal}
	case token.BINDING:
		p.next()
		return &ast.Binding{Tok: tok, Name: tok.Literal}
	case token.SELF:
		p.next()
		return &ast.Self{Tok: tok}
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
	for token.IsClause(p.cur().Type) {
		tok := p.next()
		v := p.parseClauseValue(tok.Type)
		if v == nil {
			return out
		}
		out = append(out, ast.Clause{Tok: tok, Kw: tok.Type, Value: v})
	}
	return out
}

func (p *Parser) parseClauseValue(kw token.Type) ast.Expression {
	// `with name "alice"` (spec §5.3) is a one-field record written without
	// braces. It is only that when a bare name is followed by a value; `with
	// @content` and `with { ... }` are ordinary expressions.
	if kw == token.WITH && p.curIs(token.IDENT) && startsArgument(p.peek().Type) {
		key := p.next()
		v := p.parseExpression(pipe)
		if v == nil {
			return nil
		}
		return &ast.RecordLiteral{Tok: key, Pairs: []ast.Pair{{Key: key.Literal, Value: v}}}
	}
	return p.parseExpression(pipe)
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
		token.PATH, token.URL, token.TRUE, token.FALSE,
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
