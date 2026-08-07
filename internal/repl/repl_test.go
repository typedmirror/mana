package repl

import (
	"strings"
	"testing"

	"github.com/typedmirror/mana/internal/host"
	"github.com/typedmirror/mana/internal/object"
)

func TestRunReportsFailureThroughTheExitCode(t *testing.T) {
	h := host.NewFake()
	if got := Run("-- looking for a file that is absent\n@c = read ./nope.json", h); got != ExitRuntime {
		t.Fatalf("got exit %d, want %d", got, ExitRuntime)
	}
	if !strings.Contains(h.Stderr.String(), "looking for a file that is absent") {
		t.Errorf("the intent did not reach the printed error:\n%s", h.Stderr.String())
	}
}

func TestParseErrorHasItsOwnExitCode(t *testing.T) {
	h := host.NewFake()
	if got := Run("@x = = =", h); got != ExitParse {
		t.Fatalf("got exit %d, want %d", got, ExitParse)
	}
	if !strings.Contains(h.Stderr.String(), "parse error") {
		t.Errorf("got %q", h.Stderr.String())
	}
}

func TestRunSucceeds(t *testing.T) {
	h := host.NewFake()
	if got := Run(`send "hi" to output`, h); got != ExitOK {
		t.Fatalf("got exit %d", got)
	}
	if strings.TrimSpace(h.Stdout.String()) != "hi" {
		t.Errorf("got %q", h.Stdout.String())
	}
}

// TestDiagnosticsStayOffStdout: `mana script.mana > out.json` must contain only
// what the script sent, not the runtime's commentary about it.
func TestDiagnosticsStayOffStdout(t *testing.T) {
	h := host.NewFake()
	Run("-- this will fail\n@c = read ./nope.json", h)
	if h.Stdout.String() != "" {
		t.Errorf("stdout was polluted with diagnostics: %q", h.Stdout.String())
	}
}

// TestReplKeepsBindingsAcrossEntries: the session is the context window
// (axiom 7), so a binding made on one line is visible on the next.
func TestReplKeepsBindingsAcrossEntries(t *testing.T) {
	h := host.NewFake()
	if err := Start(strings.NewReader("@x = 40\n@y = @x + 2\n"), h); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.Stdout.String(), "42") {
		t.Errorf("got %q", h.Stdout.String())
	}
}

// TestReplBuffersUntilBracketsClose: a match block typed over several lines has
// to reach the parser whole.
func TestReplBuffersUntilBracketsClose(t *testing.T) {
	h := host.NewFake()
	src := `@d = fetch https://down.example.com/x
@d |> match {
    ok _:  send "up" to output
    err _: send "down" to output
}
`
	if err := Start(strings.NewReader(src), h); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.Stdout.String(), "down") {
		t.Errorf("the multi-line match did not run:\n%s", h.Stdout.String())
	}
	if strings.Contains(h.Stderr.String(), "parse error") {
		t.Errorf("the block was parsed before it was complete:\n%s", h.Stderr.String())
	}
}

func TestReplBuffersMultiLineRecords(t *testing.T) {
	h := host.NewFake()
	src := "@c = {\n    server: { port: 8080 }\n    features: [\"auth\"]\n}\n"
	if err := Start(strings.NewReader(src), h); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.Stderr.String(), "parse error") {
		t.Errorf("got %q", h.Stderr.String())
	}
	if !strings.Contains(h.Stdout.String(), "port: 8080") {
		t.Errorf("got %q", h.Stdout.String())
	}
}

// TestReplShowsTheIntentChannel: `--` lines are reasoning, so they echo dimmed
// on the error stream where they cannot be confused with output.
func TestReplShowsTheIntentChannel(t *testing.T) {
	h := host.NewFake()
	if err := Start(strings.NewReader("-- checking the thing\n@x = 1\n"), h); err != nil {
		t.Fatal(err)
	}
	errOut := h.Stderr.String()
	if !strings.Contains(errOut, "-- checking the thing") {
		t.Errorf("intent was not echoed: %q", errOut)
	}
	if !strings.Contains(errOut, "\x1b[2m") {
		t.Errorf("intent was not dimmed: %q", errOut)
	}
	if strings.Contains(h.Stdout.String(), "checking the thing") {
		t.Errorf("intent leaked into stdout: %q", h.Stdout.String())
	}
}

// TestReplReportsAFailureOnceOnly guards against the end-of-run sweep
// re-surfacing a failure the user has already been shown.
func TestReplReportsAFailureOnceOnly(t *testing.T) {
	h := host.NewFake()
	if err := Start(strings.NewReader("@c = read ./nope.json\n@x = 1\n@y = 2\n"), h); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(h.Stderr.String(), "status: err"); got != 1 {
		t.Errorf("the same failure was reported %d times:\n%s", got, h.Stderr.String())
	}
}

func TestReplReportsParseErrorsWithoutDying(t *testing.T) {
	h := host.NewFake()
	if err := Start(strings.NewReader("@x = = =\n@y = 7\n"), h); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.Stderr.String(), "parse error") {
		t.Errorf("got %q", h.Stderr.String())
	}
	if !strings.Contains(h.Stdout.String(), "7") {
		t.Errorf("the REPL did not recover from a syntax error: %q", h.Stdout.String())
	}
}

func TestDumpTokensShowsTheIntentChannel(t *testing.T) {
	var b strings.Builder
	DumpTokens("-- reasoning\n@x = 1", &b)
	if !strings.Contains(b.String(), "INTENT") || !strings.Contains(b.String(), "reasoning") {
		t.Errorf("got %q", b.String())
	}
}

func TestUnclosedCountsTokensNotBytes(t *testing.T) {
	// A brace inside a string or a raw shell line must not open a block.
	for _, src := range []string{`@x = "a { b"`, "run echo {"} {
		if unclosed(src) {
			t.Errorf("%q was treated as an open block", src)
		}
	}
	if !unclosed("@d |> match {") {
		t.Error("a genuinely open block was not detected")
	}
}

// --- v2 run modes -------------------------------------------------------------

func TestDryRunGoesToStdoutAndCausesNothing(t *testing.T) {
	h := host.NewFake()
	code := RunWith(`act "a" {
    write "x" to ./out.json
    send 1
}`, h, Options{DryRun: true})

	if code != ExitOK {
		t.Fatalf("got exit %d", code)
	}
	if len(h.Written) != 0 {
		t.Errorf("a dry run wrote files: %+v", h.Written)
	}
	// With --dry-run the plan *is* the output, so it belongs on stdout.
	if !strings.Contains(h.Stdout.String(), "would  write") {
		t.Errorf("got %q", h.Stdout.String())
	}
}

func TestDryRunReportsAnUnrunnableGraph(t *testing.T) {
	h := host.NewFake()
	code := RunWith(`act "a" depends on "b" { send 1 }
act "b" depends on "a" { send 2 }`, h, Options{DryRun: true})
	if code != ExitRuntime {
		t.Fatalf("got exit %d", code)
	}
	if !strings.Contains(h.Stderr.String(), "dependency cycle") {
		t.Errorf("got %q", h.Stderr.String())
	}
}

// TestTraceIsCommentaryNotOutput: the trace describes the run, so it must not
// mix into what the script sent.
func TestTraceIsCommentaryNotOutput(t *testing.T) {
	h := host.NewFake()
	code := RunWith(`act "a" {
    -- doing the thing
    send "payload" to output
}`, h, Options{Trace: true})

	if code != ExitOK {
		t.Fatalf("got exit %d", code)
	}
	if strings.TrimSpace(h.Stdout.String()) != "payload" {
		t.Errorf("stdout should hold only the script's output: %q", h.Stdout.String())
	}
	if !strings.Contains(h.Stderr.String(), "Trace:") || !strings.Contains(h.Stderr.String(), "doing the thing") {
		t.Errorf("stderr should hold the trace: %q", h.Stderr.String())
	}
}

func TestRetryFlagReachesTheScheduler(t *testing.T) {
	h := host.NewFake()
	calls := 0
	h.Register("svc", func(host.Call) object.Value {
		calls++
		if calls == 1 {
			return host.Fail("transient")
		}
		return object.String("ok")
	})
	code := RunWith("use svc\n@r = svc ping\nsend @r to output", h, Options{Retries: 1})
	if code != ExitOK {
		t.Fatalf("got exit %d: %s", code, h.Stderr.String())
	}
	if calls != 2 {
		t.Errorf("calls: got %d, want 2", calls)
	}
}
