package lexer

import (
	"testing"

	"github.com/typedmirror/mana/internal/token"
)

type want struct {
	typ token.Type
	lit string
}

func check(t *testing.T, input string, wants []want) {
	t.Helper()
	got := Tokens(input)
	for i, w := range wants {
		if i >= len(got) {
			t.Fatalf("token %d: ran out of tokens, want %s(%q)", i, w.typ, w.lit)
		}
		if got[i].Type != w.typ || got[i].Literal != w.lit {
			t.Errorf("token %d: got %s(%q), want %s(%q)",
				i, got[i].Type, got[i].Literal, w.typ, w.lit)
		}
	}
}

// TestDashDisambiguation is the whole point of the '-' branch: three different
// tokens all begin with the same byte.
func TestDashDisambiguation(t *testing.T) {
	t.Run("intent", func(t *testing.T) {
		check(t, "-- pulling user data for the monthly report\n@x = 1", []want{
			{token.INTENT, "pulling user data for the monthly report"},
			{token.BINDING, "x"},
			{token.ASSIGN, "="},
			{token.NUMBER, "1"},
			{token.EOF, ""},
		})
	})

	t.Run("arrow", func(t *testing.T) {
		check(t, "@names = @users -> map name", []want{
			{token.BINDING, "names"},
			{token.ASSIGN, "="},
			{token.BINDING, "users"},
			{token.ARROW, "->"},
			{token.IDENT, "map"},
			{token.IDENT, "name"},
			{token.EOF, ""},
		})
	})

	t.Run("minus", func(t *testing.T) {
		check(t, "@n = 10 - 3", []want{
			{token.BINDING, "n"},
			{token.ASSIGN, "="},
			{token.NUMBER, "10"},
			{token.MINUS, "-"},
			{token.NUMBER, "3"},
			{token.EOF, ""},
		})
	})

	t.Run("intent at eof without trailing newline", func(t *testing.T) {
		check(t, "-- done", []want{
			{token.INTENT, "done"},
			{token.EOF, ""},
		})
	})

	t.Run("empty intent", func(t *testing.T) {
		check(t, "--\n@x = 1", []want{
			{token.INTENT, ""},
			{token.BINDING, "x"},
		})
	})
}

func TestVerbsAndClauses(t *testing.T) {
	check(t, `fetch users from "https://api.com" where role is "admin"`, []want{
		{token.FETCH, "fetch"},
		{token.IDENT, "users"},
		{token.FROM, "from"},
		{token.STRING, "https://api.com"},
		{token.WHERE, "where"},
		{token.IDENT, "role"},
		{token.IS, "is"},
		{token.STRING, "admin"},
		{token.EOF, ""},
	})
}

func TestPipeChainAndSelf(t *testing.T) {
	check(t, "@active |> map { display: @.name } |> send to output", []want{
		{token.BINDING, "active"},
		{token.PIPE, "|>"},
		{token.IDENT, "map"},
		{token.LBRACE, "{"},
		{token.IDENT, "display"},
		{token.COLON, ":"},
		{token.SELF, "@"},
		{token.DOT, "."},
		{token.IDENT, "name"},
		{token.RBRACE, "}"},
		{token.PIPE, "|>"},
		{token.SEND, "send"},
		{token.TO, "to"},
		{token.IDENT, "output"},
		{token.EOF, ""},
	})
}

func TestLiterals(t *testing.T) {
	check(t, `{ ratio: 3.14, items: [1, 2], ok: true } or false`, []want{
		{token.LBRACE, "{"},
		{token.IDENT, "ratio"},
		{token.COLON, ":"},
		{token.NUMBER, "3.14"},
		{token.COMMA, ","},
		{token.IDENT, "items"},
		{token.COLON, ":"},
		{token.LBRACKET, "["},
		{token.NUMBER, "1"},
		{token.COMMA, ","},
		{token.NUMBER, "2"},
		{token.RBRACKET, "]"},
		{token.COMMA, ","},
		{token.IDENT, "ok"},
		{token.COLON, ":"},
		{token.TRUE, "true"},
		{token.RBRACE, "}"},
		{token.OR, "or"},
		{token.FALSE, "false"},
		{token.EOF, ""},
	})
}

func TestLineNumbers(t *testing.T) {
	got := Tokens("-- first\n@x = 1\n-- second\n@y = 2")
	lines := map[token.Type]int{}
	for _, tok := range got {
		if tok.Type == token.INTENT {
			lines[token.INTENT]++
		}
	}
	if lines[token.INTENT] != 2 {
		t.Fatalf("got %d INTENT tokens, want 2", lines[token.INTENT])
	}
	if got[len(got)-1].Line != 4 {
		t.Errorf("EOF on line %d, want 4", got[len(got)-1].Line)
	}
}
