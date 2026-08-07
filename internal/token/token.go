// Package token defines the lexical vocabulary of Mana.
//
// Mana is a dual-channel language (spec §3): INTENT tokens carry the agent's
// natural-language reasoning and are preserved through every stage of the
// pipeline, while every other token belongs to the effect channel.
package token

// Type identifies a token's lexical class.
type Type string

// Token is a single lexeme plus the source position it came from.
type Token struct {
	Type    Type
	Literal string
	Line    int
}

const (
	ILLEGAL = Type("ILLEGAL")
	EOF     = Type("EOF")

	// NEWLINE terminates a statement. Mana is mostly newline-insensitive, but
	// `run` takes a raw shell line and pipe chains break across lines, so the
	// lexer decides where a break is meaningful rather than the parser.
	NEWLINE = Type("NEWLINE")

	// INTENT is a `--` line. Not a comment: the evaluator pushes it onto the
	// intent stack so failures can report the reasoning that preceded them
	// (spec §15.3).
	INTENT = Type("INTENT")

	// RAW is everything after `run` up to end of line — the shell command,
	// verbatim. Spec §5.2: "No escaping, no subprocess wrapper."
	RAW = Type("RAW")

	IDENT   = Type("IDENT")   // filter, name, count
	BINDING = Type("BINDING") // @users -> literal "users"
	SELF    = Type("SELF")    // @ inside a transform (spec §7.3)
	NUMBER  = Type("NUMBER")
	STRING  = Type("STRING")

	// PATH and URL are bare words the spec writes unquoted: `read ./config.json`,
	// `fetch https://api.example.com/health`. They evaluate to strings; the
	// separate types exist so an error can say which one failed to resolve.
	PATH = Type("PATH")
	URL  = Type("URL")

	ASSIGN = Type("=")
	PIPE   = Type("|>")
	ARROW  = Type("->")

	PLUS     = Type("+")
	MINUS    = Type("-")
	SLASH    = Type("/")
	ASTERISK = Type("*")
	LT       = Type("<")
	GT       = Type(">")
	LTE      = Type("<=")
	GTE      = Type(">=")
	EQ       = Type("==")
	NOTEQ    = Type("!=")

	COLON = Type(":")
	COMMA = Type(",")
	DOT   = Type(".")

	LPAREN   = Type("(")
	RPAREN   = Type(")")
	LBRACE   = Type("{")
	RBRACE   = Type("}")
	LBRACKET = Type("[")
	RBRACKET = Type("]")

	// Verbs (spec §5). Seven execution primitives, one per I/O boundary.
	FETCH  = Type("FETCH")
	READ   = Type("READ")
	RUN    = Type("RUN")
	CREATE = Type("CREATE")
	WRITE  = Type("WRITE")
	SEND   = Type("SEND")
	ASK    = Type("ASK")

	// Clause keywords (spec §6). Resolved by keyword, never by position.
	WHERE = Type("WHERE")
	WITH  = Type("WITH")
	FROM  = Type("FROM")
	TO    = Type("TO")
	AS    = Type("AS")
	AT    = Type("AT")
	BY    = Type("BY")

	// Control and literals.
	OR    = Type("OR")
	IF    = Type("IF")
	THEN  = Type("THEN")
	ELSE  = Type("ELSE")
	MATCH = Type("MATCH")
	IS    = Type("IS")
	TRUE  = Type("TRUE")
	FALSE = Type("FALSE")
)

// keywords maps reserved words to their token type. Every entry is a
// high-frequency English word — axiom 1: LLMs generate text, so the syntax
// should sit on tokens they already emit constantly.
var keywords = map[string]Type{
	"fetch":  FETCH,
	"read":   READ,
	"run":    RUN,
	"create": CREATE,
	"write":  WRITE,
	"send":   SEND,
	"ask":    ASK,

	"where": WHERE,
	"with":  WITH,
	"from":  FROM,
	"to":    TO,
	"as":    AS,
	"at":    AT,
	"by":    BY,

	"or":    OR,
	"if":    IF,
	"then":  THEN,
	"else":  ELSE,
	"match": MATCH,
	"is":    IS,
	"true":  TRUE,
	"false": FALSE,
}

// LookupIdent returns the keyword type for ident, or IDENT if it is not
// reserved. Transform names (filter, map, sort, count, ...) deliberately stay
// IDENT so the evaluator can resolve them alongside ambient tools (spec §11).
func LookupIdent(ident string) Type {
	if t, ok := keywords[ident]; ok {
		return t
	}
	return IDENT
}

// IsVerb reports whether t is one of the seven execution primitives.
func IsVerb(t Type) bool {
	switch t {
	case FETCH, READ, RUN, CREATE, WRITE, SEND, ASK:
		return true
	}
	return false
}

// IsClause reports whether t introduces a verb clause (spec §6).
func IsClause(t Type) bool {
	switch t {
	case WHERE, WITH, FROM, TO, AS, AT, BY:
		return true
	}
	return false
}
