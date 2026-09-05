// Package lexer turns Mana source text into a stream of tokens.
//
// Three lexical facts drive the design, and all three come from the spec rather
// than from convenience:
//
//   - `--` is INTENT, not a comment (§3). It runs to end of line, because prose
//     ends at the line break.
//   - `run` takes a raw shell line (§5.2): "No escaping, no subprocess wrapper."
//     So the lexer switches to a raw mode for exactly one line after `run`.
//   - Paths and URLs appear unquoted (`read ./config.json`, §5) so they are
//     lexed as single words instead of shattering into DOT SLASH IDENT.
//
// Because of the last two, the lexer cannot be newline-blind. It decides where
// a line break terminates a statement and emits NEWLINE only there; everywhere
// else — mid-record, mid-pipe-chain, mid-fallback — the break is whitespace.
package lexer

import (
	"strings"

	"github.com/typedmirror/mana/internal/token"
)

// Lexer scans a source string one byte at a time.
type Lexer struct {
	input   string
	pos     int  // index of ch
	readPos int  // index of the byte after ch
	ch      byte // byte under examination; 0 means EOF
	line    int

	prev    token.Type // type of the last token emitted; drives newline handling
	rawNext bool       // the next token is a raw shell line (set by `run`)
}

// New returns a Lexer positioned on the first byte of input.
func New(input string) *Lexer {
	l := &Lexer{input: input, line: 1}
	l.readChar()
	return l
}

// Tokens drains the lexer into a slice, EOF included. Convenient for tests and
// for the token-dump mode of the CLI.
func Tokens(input string) []token.Token {
	l := New(input)
	var out []token.Token
	for {
		tok := l.NextToken()
		out = append(out, tok)
		if tok.Type == token.EOF {
			return out
		}
	}
}

// NextToken scans and returns the next token.
func (l *Lexer) NextToken() token.Token {
	tok := l.scan()
	l.prev = tok.Type
	return tok
}

func (l *Lexer) scan() token.Token {
	if l.rawNext {
		l.rawNext = false
		if tok, ok := l.readRawLine(); ok {
			return tok
		}
	}

	if tok, ok := l.skipToSignificant(); ok {
		return tok // a meaningful line break
	}
	line := l.line

	switch l.ch {
	case 0:
		return token.Token{Type: token.EOF, Literal: "", Line: line}

	case '-':
		// Three tokens start with '-', and the order of these checks is the
		// disambiguation: `--` INTENT, then `->` ARROW, then bare MINUS.
		switch l.peekChar() {
		case '-':
			return token.Token{Type: token.INTENT, Literal: l.readIntent(), Line: line}
		case '>':
			l.readChar()
			l.readChar()
			return token.Token{Type: token.ARROW, Literal: "->", Line: line}
		}
		return l.single(token.MINUS, line)

	case '@':
		// `@name` binds or references; a bare `@` is the self-reference used
		// inside transforms, where `@.name` lexes as SELF DOT IDENT.
		if isLetter(l.peekChar()) {
			l.readChar()
			return token.Token{Type: token.BINDING, Literal: l.readIdentifier(), Line: line}
		}
		l.readChar()
		return token.Token{Type: token.SELF, Literal: "@", Line: line}

	case '|':
		if l.peekChar() == '>' {
			l.readChar()
			l.readChar()
			return token.Token{Type: token.PIPE, Literal: "|>", Line: line}
		}
		return l.single(token.ILLEGAL, line)

	case '"':
		lit, ok := l.readString()
		if !ok {
			return token.Token{Type: token.ILLEGAL, Literal: lit, Line: line}
		}
		return token.Token{Type: token.STRING, Literal: lit, Line: line}

	case '.':
		// `./x` and `../x` are paths; a lone '.' is field access.
		if l.peekChar() == '/' || (l.peekChar() == '.' && l.peekAt(2) == '/') {
			return token.Token{Type: token.PATH, Literal: l.readBareWord(), Line: line}
		}
		return l.single(token.DOT, line)

	case '/':
		// `/tmp/test` is a path; `a / b` is division. The discriminator is the
		// space, which is why the spec's own examples always space their
		// arithmetic and never space their paths.
		if isWordByte(l.peekChar()) {
			return token.Token{Type: token.PATH, Literal: l.readBareWord(), Line: line}
		}
		return l.single(token.SLASH, line)

	case '~':
		if l.peekChar() == '/' {
			return token.Token{Type: token.PATH, Literal: l.readBareWord(), Line: line}
		}
		return l.single(token.ILLEGAL, line)

	case '=':
		if l.peekChar() == '=' {
			return l.pair(token.EQ, line)
		}
		return l.single(token.ASSIGN, line)
	case '!':
		if l.peekChar() == '=' {
			return l.pair(token.NOTEQ, line)
		}
		return l.single(token.ILLEGAL, line)
	case '+':
		return l.single(token.PLUS, line)
	case '*':
		return l.single(token.ASTERISK, line)
	case '<':
		if l.peekChar() == '=' {
			return l.pair(token.LTE, line)
		}
		return l.single(token.LT, line)
	case '>':
		if l.peekChar() == '=' {
			return l.pair(token.GTE, line)
		}
		return l.single(token.GT, line)
	case ':':
		return l.single(token.COLON, line)
	case ',':
		return l.single(token.COMMA, line)
	case '(':
		return l.single(token.LPAREN, line)
	case ')':
		return l.single(token.RPAREN, line)
	case '{':
		return l.single(token.LBRACE, line)
	case '}':
		return l.single(token.RBRACE, line)
	case '[':
		return l.single(token.LBRACKET, line)
	case ']':
		return l.single(token.RBRACKET, line)
	}

	switch {
	case isLetter(l.ch):
		start := l.pos
		lit := l.readIdentifier()
		// `act.<name>.result` is lexed whole. Act names are hyphenated
		// throughout the spec, so read as ordinary tokens
		// `act.check-inventory.result` becomes a subtraction — which parses,
		// and is wrong.
		if lit == "act" && l.ch == '.' {
			if name, ok := l.readActRef(); ok {
				return token.Token{Type: token.ACTREF, Literal: name, Line: line}
			}
		}
		// `https://...` — an identifier immediately followed by "://" is a
		// scheme, not a name. Rewind conceptually and take the whole word.
		if l.ch == ':' && l.peekChar() == '/' && l.peekAt(2) == '/' {
			l.readBareWord()
			return token.Token{Type: token.URL, Literal: l.input[start:l.pos], Line: line}
		}
		typ := token.LookupIdent(lit)
		if typ == token.RUN && !l.runIsToolCall() {
			l.rawNext = true
		}
		return token.Token{Type: typ, Literal: lit, Line: line}
	case isDigit(l.ch):
		lit := l.readNumber()
		return token.Token{Type: token.NUMBER, Literal: lit, Line: line}
	default:
		return l.single(token.ILLEGAL, line)
	}
}

// readActRef consumes `.<name>.result` after the word `act`, returning the act
// name. It reports false and rewinds if the shape does not match, so a plain
// field access on something called `act` is unaffected.
func (l *Lexer) readActRef() (string, bool) {
	mark := *l

	l.readChar() // past '.'
	start := l.pos
	for isLetter(l.ch) || isDigit(l.ch) || l.ch == '-' {
		l.readChar()
	}
	name := l.input[start:l.pos]

	if name == "" || !l.wordAhead(".result") {
		*l = mark
		return "", false
	}
	for range ".result" {
		l.readChar()
	}
	return name, true
}

// runIsToolCall distinguishes `run tool search_web with ...` (spec §11, a named
// tool) from `run curl -s http://...` (spec §5.2, raw shell). Only the literal
// word `tool` steers away from raw mode.
func (l *Lexer) runIsToolCall() bool {
	i := l.pos
	for i < len(l.input) && (l.input[i] == ' ' || l.input[i] == '\t') {
		i++
	}
	rest := l.input[i:]
	if !strings.HasPrefix(rest, "tool") {
		return false
	}
	return len(rest) == 4 || !isLetter(rest[4])
}

// readRawLine consumes the shell command after `run`. It reports false when the
// line is empty, so the parser sees a missing command rather than an empty one.
func (l *Lexer) readRawLine() (token.Token, bool) {
	for l.ch == ' ' || l.ch == '\t' {
		l.readChar()
	}
	line := l.line
	start := l.pos
	for l.ch != '\n' && l.ch != 0 {
		l.readChar()
	}
	lit := strings.TrimSpace(l.input[start:l.pos])
	if lit == "" {
		return token.Token{}, false
	}
	return token.Token{Type: token.RAW, Literal: lit, Line: line}, true
}

// skipToSignificant advances past whitespace, returning a NEWLINE token if one
// of the line breaks it crossed actually terminates a statement.
func (l *Lexer) skipToSignificant() (token.Token, bool) {
	for {
		for l.ch == ' ' || l.ch == '\t' || l.ch == '\r' {
			l.readChar()
		}
		if l.ch != '\n' {
			return token.Token{}, false
		}
		line := l.line
		for l.ch == '\n' || l.ch == ' ' || l.ch == '\t' || l.ch == '\r' {
			l.readChar()
		}
		if l.breakIsSignificant() {
			return token.Token{Type: token.NEWLINE, Literal: "\n", Line: line}, true
		}
	}
}

// breakIsSignificant decides whether the line break just crossed ends a
// statement. It ends one when the previous token could finish a statement and
// the next one cannot continue it — so a record spread over lines, a pipe chain
// broken before `|>`, and a fallback chain broken before `or` all stay whole.
func (l *Lexer) breakIsSignificant() bool {
	switch l.prev {
	case token.IDENT, token.BINDING, token.SELF, token.NUMBER, token.STRING,
		token.PATH, token.URL, token.RAW, token.TRUE, token.FALSE, token.ACTREF,
		token.RPAREN, token.RBRACKET, token.RBRACE:
	default:
		// INTENT lands here deliberately: it already consumed to end of line,
		// so it is self-terminating and a following NEWLINE would be noise.
		return false
	}
	switch {
	case l.ch == '|' && l.peekChar() == '>':
		return false
	case l.ch == '-' && l.peekChar() == '>':
		return false
	case l.wordAhead("or"):
		return false
	// A branch written across lines is one statement. `then` and `else` can
	// only ever continue an `if`, so a break in front of either is layout.
	case l.wordAhead("then"), l.wordAhead("else"):
		return false
	// A line cannot begin with a clause, so a break before `with` is layout
	// too — which is what lets a raw `run` line take a clause at all (D-060):
	// the line belongs to the shell, the continuation belongs to mana.
	case l.wordAhead("with"):
		return false
	}
	return true
}

// wordAhead reports whether the input at the cursor is exactly word, followed by
// something that cannot extend an identifier.
func (l *Lexer) wordAhead(word string) bool {
	rest := l.input[l.pos:]
	if !strings.HasPrefix(rest, word) {
		return false
	}
	n := len(word)
	return len(rest) == n || !(isLetter(rest[n]) || isDigit(rest[n]))
}

// single emits a one-byte token and advances past it.
func (l *Lexer) single(t token.Type, line int) token.Token {
	tok := token.Token{Type: t, Literal: string(l.ch), Line: line}
	l.readChar()
	return tok
}

// pair emits a two-byte token and advances past both bytes.
func (l *Lexer) pair(t token.Type, line int) token.Token {
	first := l.ch
	l.readChar()
	tok := token.Token{Type: t, Literal: string([]byte{first, l.ch}), Line: line}
	l.readChar()
	return tok
}

func (l *Lexer) readChar() {
	if l.readPos >= len(l.input) {
		l.ch = 0
	} else {
		l.ch = l.input[l.readPos]
	}
	if l.ch == '\n' {
		l.line++
	}
	l.pos = l.readPos
	l.readPos++
}

func (l *Lexer) peekChar() byte { return l.peekAt(1) }

// peekAt returns the byte n positions past the current one, or 0 past the end.
func (l *Lexer) peekAt(n int) byte {
	i := l.pos + n
	if i >= len(l.input) {
		return 0
	}
	return l.input[i]
}

// readIntent consumes a `--` line and returns its text with surrounding
// whitespace stripped. It assumes l.ch is the first '-' of the pair, and
// leaves the lexer on the terminating newline (or EOF).
func (l *Lexer) readIntent() string {
	l.readChar() // past the first '-'
	l.readChar() // past the second '-'
	start := l.pos
	for l.ch != '\n' && l.ch != 0 {
		l.readChar()
	}
	return strings.TrimSpace(l.input[start:l.pos])
}

func (l *Lexer) readIdentifier() string {
	start := l.pos
	for isLetter(l.ch) || isDigit(l.ch) {
		l.readChar()
	}
	return l.input[start:l.pos]
}

func (l *Lexer) readNumber() string {
	start := l.pos
	for isDigit(l.ch) {
		l.readChar()
	}
	if l.ch == '.' && isDigit(l.peekChar()) {
		l.readChar()
		for isDigit(l.ch) {
			l.readChar()
		}
	}
	return l.input[start:l.pos]
}

// readBareWord consumes an unquoted path or URL. It stops at whitespace and at
// the punctuation that can only be structure — a trailing `,` or `]` belongs to
// the enclosing literal, never to the path.
func (l *Lexer) readBareWord() string {
	start := l.pos
	for isWordByte(l.ch) {
		l.readChar()
	}
	return l.input[start:l.pos]
}

// readString consumes a double-quoted string. Spec §13 defines STRING as
// '"' [^"]* '"' — no escape sequences in v0.1.
func (l *Lexer) readString() (string, bool) {
	l.readChar() // past the opening quote
	start := l.pos
	for l.ch != '"' && l.ch != 0 {
		l.readChar()
	}
	if l.ch == 0 {
		return l.input[start:l.pos], false // unterminated
	}
	lit := l.input[start:l.pos]
	l.readChar() // past the closing quote
	return lit, true
}

func isLetter(ch byte) bool {
	return 'a' <= ch && ch <= 'z' || 'A' <= ch && ch <= 'Z' || ch == '_'
}

func isDigit(ch byte) bool {
	return '0' <= ch && ch <= '9'
}

// isWordByte reports whether ch can appear inside a bare path or URL.
func isWordByte(ch byte) bool {
	switch ch {
	case 0, ' ', '\t', '\r', '\n', ',', '(', ')', '[', ']', '{', '}', '"':
		return false
	}
	return true
}
