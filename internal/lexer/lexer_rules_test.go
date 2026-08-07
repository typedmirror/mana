package lexer

import (
	"testing"

	"github.com/typedmirror/mana/internal/token"
)

// TestRawShellLine is spec §5.2. Flags and paths inside a `run` must reach the
// shell untouched; lexing `-o` as MINUS IDENT would already have broken it.
func TestRawShellLine(t *testing.T) {
	check(t, "run gcc -o /tmp/test /tmp/test.c\n@x = 1", []want{
		{token.RUN, "run"},
		{token.RAW, "gcc -o /tmp/test /tmp/test.c"},
		{token.NEWLINE, "\n"},
		{token.BINDING, "x"},
		{token.ASSIGN, "="},
		{token.NUMBER, "1"},
		{token.EOF, ""},
	})
}

// TestRunToolIsNotShell: spec §11 writes `run tool <name>`, which must lex as
// ordinary tokens so the clause parser can see it.
func TestRunToolIsNotShell(t *testing.T) {
	check(t, `run tool search_web with query "spec"`, []want{
		{token.RUN, "run"},
		{token.IDENT, "tool"},
		{token.IDENT, "search_web"},
		{token.WITH, "with"},
		{token.IDENT, "query"},
		{token.STRING, "spec"},
		{token.EOF, ""},
	})
}

func TestBarePathsAndURLs(t *testing.T) {
	check(t, "read ./config.json", []want{
		{token.READ, "read"},
		{token.PATH, "./config.json"},
	})
	check(t, "fetch https://api.example.com/health", []want{
		{token.FETCH, "fetch"},
		{token.URL, "https://api.example.com/health"},
	})
	check(t, "write ../out/x.csv @d", []want{
		{token.WRITE, "write"},
		{token.PATH, "../out/x.csv"},
		{token.BINDING, "d"},
	})
}

// TestSlashStaysDivisionWhenSpaced is the discriminator between a path and an
// arithmetic operator. Spec §14 relies on both in the same record.
func TestSlashStaysDivisionWhenSpaced(t *testing.T) {
	check(t, "@r = @a / @b", []want{
		{token.BINDING, "r"},
		{token.ASSIGN, "="},
		{token.BINDING, "a"},
		{token.SLASH, "/"},
		{token.BINDING, "b"},
	})
}

// TestLineBreaksThatDoNotEndAStatement: a chain broken before `|>` or `or`, and
// a record spread across lines, must stay one statement.
func TestLineBreaksThatDoNotEndAStatement(t *testing.T) {
	got := Tokens("@d\n  |> filter where active\n  |> count")
	for _, tok := range got {
		if tok.Type == token.NEWLINE {
			t.Fatalf("a pipe chain was split by a NEWLINE: %v", render(got))
		}
	}

	got = Tokens("@c = read ./a.json\n  or read ./b.json")
	for _, tok := range got {
		if tok.Type == token.NEWLINE {
			t.Fatalf("a fallback chain was split by a NEWLINE: %v", render(got))
		}
	}
}

func TestLineBreaksThatDoEndAStatement(t *testing.T) {
	got := Tokens("@x = 1\n@y = 2")
	if countType(got, token.NEWLINE) != 1 {
		t.Fatalf("want exactly one NEWLINE, got %v", render(got))
	}
}

// TestRecordAcrossLinesSeparatesPairs: spec §9's nested record uses line breaks
// instead of commas, so the break has to survive as a separator.
func TestRecordAcrossLinesSeparatesPairs(t *testing.T) {
	// Two breaks are emitted: one between the pairs, which the parser needs as
	// a separator, and one before the closing brace, which it skips.
	src := "@c = {\n    server: { port: 8080 }\n    features: [\"auth\"]\n}"
	if got := countType(Tokens(src), token.NEWLINE); got != 2 {
		t.Fatalf("want a NEWLINE between the pairs and before the closer, got %d: %v", got, render(Tokens(src)))
	}
}

func TestUnterminatedStringIsIllegal(t *testing.T) {
	got := Tokens(`@x = "oops`)
	if got[len(got)-2].Type != token.ILLEGAL {
		t.Fatalf("want ILLEGAL for an unterminated string, got %v", render(got))
	}
}

func countType(toks []token.Token, t token.Type) int {
	n := 0
	for _, tok := range toks {
		if tok.Type == t {
			n++
		}
	}
	return n
}

func render(toks []token.Token) []string {
	out := make([]string, len(toks))
	for i, tok := range toks {
		out[i] = string(tok.Type) + "(" + tok.Literal + ")"
	}
	return out
}
