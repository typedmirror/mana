// Package repl provides the interactive loop and the script runner.
//
// Both paths run the same pipeline (spec §15.2) — lex, parse, evaluate — and
// differ only in how much source arrives at once and whether state survives
// between entries.
package repl

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/typedmirror/mana/internal/evaluator"
	"github.com/typedmirror/mana/internal/host"
	"github.com/typedmirror/mana/internal/lexer"
	"github.com/typedmirror/mana/internal/object"
	"github.com/typedmirror/mana/internal/parser"
	"github.com/typedmirror/mana/internal/token"
)

const (
	prompt    = "mana> "
	continued = "  ... "
)

// Exit codes. A caller upstream of Mana can see nothing else, so the code has
// to distinguish "your script is wrong" from "your script ran and failed".
const (
	ExitOK      = 0
	ExitRuntime = 1
	ExitParse   = 2
	ExitNoFile  = 3
)

// dim wraps text in the ANSI faint attribute so the intent channel reads as
// commentary rather than output.
func dim(s string) string { return "\x1b[2m" + s + "\x1b[0m" }

// Run executes a whole script and returns the process exit code. Diagnostics go
// to the host's error stream; only what the script `send`s reaches stdout.
func Run(src string, h host.Host) int {
	p := parser.New(src)
	prog := p.Parse()
	if errs := p.Errors(); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintln(h.Err(), "parse error: "+e)
		}
		return ExitParse
	}
	if err, bad := evaluator.New(h).Run(prog).(*object.Err); bad {
		fmt.Fprintln(h.Err(), err.Inspect())
		return ExitRuntime
	}
	return ExitOK
}

// Start runs the interactive loop until in is exhausted. Bindings persist
// across entries: the context window is the scope (axiom 7), and in a REPL the
// session is the context window.
func Start(in io.Reader, h host.Host) error {
	scanner := bufio.NewScanner(in)
	e := evaluator.New(h)
	e.OnIntent = func(text string) {
		fmt.Fprintln(h.Err(), dim("-- "+text))
	}
	out := h.Out()

	var buf strings.Builder
	for {
		if buf.Len() == 0 {
			fmt.Fprint(out, prompt)
		} else {
			fmt.Fprint(out, continued)
		}
		if !scanner.Scan() {
			fmt.Fprintln(out)
			return scanner.Err()
		}
		buf.WriteString(scanner.Text())
		buf.WriteString("\n")

		// A `match` block or a record spans lines. Keep reading until the
		// brackets close so the parser sees a whole construct, not a truncated
		// one that would report a syntax error the user did not make.
		if unclosed(buf.String()) {
			continue
		}
		src := buf.String()
		buf.Reset()
		evalOne(e, src, h)
	}
}

func evalOne(e *evaluator.Evaluator, src string, h host.Host) {
	p := parser.New(src)
	prog := p.Parse()
	if errs := p.Errors(); len(errs) > 0 {
		for _, err := range errs {
			fmt.Fprintln(h.Err(), "parse error: "+err)
		}
		return
	}
	result := e.Run(prog)
	if err, bad := result.(*object.Err); bad {
		fmt.Fprintln(h.Err(), err.Inspect())
		// Reported once. Without this the end-of-run sweep would surface the
		// same failure again after every subsequent entry.
		err.Handle()
		return
	}
	if _, isNull := result.(object.Null); !isNull {
		fmt.Fprintln(h.Out(), result.Inspect())
	}
}

// unclosed reports whether src has an open bracket still waiting. It counts
// tokens rather than bytes so a bracket inside a string or a `run` line does
// not confuse it.
func unclosed(src string) bool {
	depth := 0
	for _, tok := range lexer.Tokens(src) {
		switch tok.Type {
		case token.LBRACE, token.LBRACKET, token.LPAREN:
			depth++
		case token.RBRACE, token.RBRACKET, token.RPAREN:
			depth--
		}
	}
	return depth > 0
}

// DumpTokens prints the token stream, including the INTENT tokens. Useful for
// seeing that the dual channel survived lexing.
func DumpTokens(src string, out io.Writer) {
	for _, tok := range lexer.Tokens(src) {
		if tok.Type == token.EOF {
			return
		}
		fmt.Fprintf(out, "%3d  %-8s %s\n", tok.Line, tok.Type, tok.Literal)
	}
}
