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
	"time"

	"github.com/typedmirror/mana/internal/act"
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

// Options control one script run.
type Options struct {
	Retries int           // extra attempts for a failed act
	Trace   bool          // print the execution record afterwards
	DryRun  bool          // report what would happen, cause nothing
	JSON    bool          // emit the report as JSON instead of running normally
	Timeout time.Duration // bound on each shell command; zero uses the default
}

// Run executes a whole script and returns the process exit code. Diagnostics go
// to the host's error stream; only what the script `send`s reaches stdout.
func Run(src string, h host.Host) int { return RunWith(src, h, Options{}) }

// RunWith executes a script with options.
func RunWith(src string, h host.Host, opts Options) int {
	p := parser.New(src)
	prog := p.Parse()
	if errs := p.Errors(); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintln(h.Err(), "parse error: "+e)
		}
		return ExitParse
	}

	if opts.DryRun {
		plan, err := act.DryRun(prog, h)
		if err != nil {
			fmt.Fprintln(h.Err(), err.Inspect())
			return ExitRuntime
		}
		// The plan goes to stdout: with --dry-run it *is* the output, and the
		// script produced none of its own.
		fmt.Fprint(h.Out(), plan.String())
		return ExitOK
	}

	// With --json the report is the answer, so the script's own output is
	// gathered into it rather than interleaved with it on the same stream.
	runHost := h
	var captured *host.Capture
	if opts.JSON {
		captured = host.NewCapture(h)
		runHost = captured
	}

	report := act.RunWith(prog, runHost, act.Options{Retries: opts.Retries, Timeout: opts.Timeout})

	if opts.JSON {
		blob, err := act.JSON(report, captured.Text())
		if err != nil {
			fmt.Fprintln(h.Err(), "could not encode the report: "+err.Error())
			return ExitRuntime
		}
		fmt.Fprintln(h.Out(), string(blob))
		if !report.OK() {
			return ExitRuntime
		}
		return ExitOK
	}

	if opts.Trace {
		// Commentary about the run, not the run's output.
		fmt.Fprint(h.Err(), act.Trace(report))
	}
	if report.Err != nil {
		fmt.Fprintln(h.Err(), report.Err.Inspect())
		return ExitRuntime
	}
	for _, o := range report.Outcomes {
		switch o.Status {
		case act.Failed:
			fmt.Fprintln(h.Err(), label(o.Name)+o.Err.Inspect())
		case act.Skipped:
			// Not a success. An act that never ran because a dependency broke
			// has to be visible, or the job reads as partially fine.
			fmt.Fprintf(h.Err(), "%sskipped — %s\n", label(o.Name), o.Reason)
		}
	}
	if !report.OK() {
		return ExitRuntime
	}
	return ExitOK
}

// EmitEnvelope prints the per-act capability envelope family (D-055),
// causing nothing — the producer half of the harmonic seam.
func EmitEnvelope(src string, h host.Host) int {
	p := parser.New(src)
	prog := p.Parse()
	if errs := p.Errors(); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintln(h.Err(), "parse error: "+e)
		}
		return ExitParse
	}
	out, err := act.Envelopes(prog, h)
	if err != nil {
		fmt.Fprintln(h.Err(), err.Inspect())
		return ExitRuntime
	}
	fmt.Fprintln(h.Out(), string(out))
	return ExitOK
}

// label prefixes a diagnostic with the act it came from. A flat script has no
// name, so it gets no prefix.
func label(name string) string {
	if name == "" {
		return ""
	}
	return "act " + name + ": "
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
