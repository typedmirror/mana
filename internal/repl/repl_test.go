package repl

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

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

func TestJSONReportGathersOutputAndSteps(t *testing.T) {
	h := host.NewFake()
	h.Shells["echo hi"] = host.Shell{Stdout: "hi"}
	code := RunWith("-- step one\n@a = run echo hi\n\n-- step two\nsend @a to output", h,
		Options{JSON: true})
	if code != ExitOK {
		t.Fatalf("got exit %d: %s", code, h.Stderr.String())
	}
	var doc struct {
		OK     bool   `json:"ok"`
		Output string `json:"output"`
		Acts   []struct {
			Steps []struct {
				Intent string `json:"intent"`
				Status string `json:"status"`
			} `json:"steps"`
		} `json:"acts"`
	}
	if err := json.Unmarshal([]byte(h.Stdout.String()), &doc); err != nil {
		t.Fatalf("the report is not valid JSON: %v\n%s", err, h.Stdout.String())
	}
	if !doc.OK {
		t.Error("job should have succeeded")
	}
	// The script's own output belongs inside the document, not beside it.
	if strings.TrimSpace(doc.Output) != "hi" {
		t.Errorf("output: got %q", doc.Output)
	}
	if len(doc.Acts) != 1 || len(doc.Acts[0].Steps) != 2 {
		t.Fatalf("got %+v", doc.Acts)
	}
	if doc.Acts[0].Steps[0].Intent != "step one" {
		t.Errorf("got %q", doc.Acts[0].Steps[0].Intent)
	}
}

func TestJSONIsTheWholeAnswerOnStdout(t *testing.T) {
	h := host.NewFake()
	RunWith(`send "payload" to output`, h, Options{JSON: true})
	// Exactly one document: the script's output must not be interleaved.
	if err := json.Unmarshal([]byte(h.Stdout.String()), &map[string]any{}); err != nil {
		t.Errorf("stdout is not a single JSON document: %v\n%s", err, h.Stdout.String())
	}
}

func TestJSONCarriesAFailureAndExitsNonZero(t *testing.T) {
	h := host.NewFake()
	code := RunWith("-- reaching for a file that is not there\n@c = read ./absent.json", h, Options{JSON: true})
	if code != ExitRuntime {
		t.Fatalf("got exit %d", code)
	}
	var doc struct {
		OK       bool `json:"ok"`
		Failures []struct {
			Intent string `json:"intent"`
			Reason string `json:"reason"`
		} `json:"failures"`
		Acts []struct {
			Status string `json:"status"`
		} `json:"acts"`
	}
	if err := json.Unmarshal([]byte(h.Stdout.String()), &doc); err != nil {
		t.Fatalf("%v\n%s", err, h.Stdout.String())
	}
	if doc.OK {
		t.Error("ok should be false")
	}
	if len(doc.Failures) != 1 || doc.Failures[0].Intent != "reaching for a file that is not there" {
		t.Errorf("the intent must survive into the triage list: %+v", doc.Failures)
	}
	if doc.Acts[0].Status != "failed" {
		t.Errorf("act status: %+v", doc.Acts)
	}
	// One failure, one place (D-048): the acts and steps carry status only.
	if n := strings.Count(h.Stdout.String(), `"reason"`); n != 1 {
		t.Errorf("the failure appears %d times, want exactly 1:\n%s", n, h.Stdout.String())
	}
}

// TestJSONMarksTheHaltedStep: a failure bound in one step stops the run in a
// later step. The later step never accomplished its intent, so reporting it
// "ok" would be a lie; it halted (D-048).
func TestJSONMarksTheHaltedStep(t *testing.T) {
	h := host.NewFake()
	code := RunWith("-- the step that creates the failure\n@c = read ./absent.json\n\n-- the step the failure stops\nsend @c to output", h, Options{JSON: true})
	if code != ExitRuntime {
		t.Fatalf("got exit %d", code)
	}
	var doc struct {
		Acts []struct {
			Steps []struct {
				Intent string `json:"intent"`
				Status string `json:"status"`
			} `json:"steps"`
		} `json:"acts"`
	}
	if err := json.Unmarshal([]byte(h.Stdout.String()), &doc); err != nil {
		t.Fatalf("%v\n%s", err, h.Stdout.String())
	}
	steps := doc.Acts[0].Steps
	if len(steps) != 2 {
		t.Fatalf("got %+v", steps)
	}
	if steps[0].Status != "failed" || steps[1].Status != "halted" {
		t.Errorf("want failed then halted, got %q then %q", steps[0].Status, steps[1].Status)
	}
}

// TestJSONRecordsEffects: the report says what actually fired, under the step
// whose reasoning fired it — the first question after a partial failure.
func TestJSONRecordsEffects(t *testing.T) {
	h := host.NewFake()
	h.Shells["touch a"] = host.Shell{Stdout: ""}
	code := RunWith("-- mutate, then fail\n@a = run touch a\n@b = write @a to ./out.txt\n@c = read ./absent.json\nsend @c to output", h, Options{JSON: true})
	if code != ExitRuntime {
		t.Fatalf("got exit %d: %s", code, h.Stdout.String())
	}
	var doc struct {
		Acts []struct {
			Steps []struct {
				Effects []string `json:"effects"`
			} `json:"steps"`
		} `json:"acts"`
	}
	if err := json.Unmarshal([]byte(h.Stdout.String()), &doc); err != nil {
		t.Fatalf("%v\n%s", err, h.Stdout.String())
	}
	effects := doc.Acts[0].Steps[0].Effects
	if len(effects) != 2 || effects[0] != "run: touch a" || effects[1] != "write: ./out.txt" {
		t.Errorf("effects: %v", effects)
	}
}

// TestDenialIsRecognizedAsData (D-058): a harmonic capability-guard refusal
// arriving in a failure's reason gains recovery guidance carrying the
// provenance ref — a denial is a programmable condition, not a dead cell.
func TestDenialIsRecognizedAsData(t *testing.T) {
	h := host.NewFake()
	h.Shells["curl api.internal"] = host.Shell{
		Code:   1,
		Stderr: "denied by capability envelope harmonic:01ABCDEF@tip42 (network not granted)",
	}
	code := RunWith("-- reaching for the api under an attenuated grant\n@a = run curl api.internal\nsend @a to output", h, Options{JSON: true})
	if code != ExitRuntime {
		t.Fatalf("got exit %d", code)
	}
	out := h.Stdout.String()
	if !strings.Contains(out, "harmonic:01ABCDEF@tip42") {
		t.Errorf("the provenance ref must survive into the report:\n%s", out)
	}
	if !strings.Contains(out, "catch it with") {
		t.Errorf("the denial must carry recovery guidance:\n%s", out)
	}
}

// And the payoff: `or` catches a denial, so an attenuated envelope degrades
// the job instead of killing it.
func TestDenialDegradesWithOr(t *testing.T) {
	h := host.NewFake()
	h.Files["./cache.json"] = `{ "cached": true }`
	h.Shells["curl api.internal"] = host.Shell{
		Code:   1,
		Stderr: "denied by capability envelope harmonic:01ABCDEF@tip42",
	}
	code := RunWith("-- live if granted, cache if not\n@a = run curl api.internal\n      or read ./cache.json as json\nsend @a to output", h, Options{})
	if code != ExitOK {
		t.Fatalf("the fallback should have carried it: exit %d\n%s", code, h.Stderr.String())
	}
	if !strings.Contains(h.Stdout.String(), "cached") {
		t.Errorf("got %q", h.Stdout.String())
	}
}

// TestReadsAreWitnessed (D-057): observed paths and URLs appear in their own
// column, apart from wreckage.
func TestReadsAreWitnessed(t *testing.T) {
	h := host.NewFake()
	h.Files["./in.json"] = `1`
	h.Responses["https://api.example.com/x"] = `2`
	code := RunWith("-- observing the world\n@a = read ./in.json\n@b = fetch https://api.example.com/x\nsend @a to output", h, Options{JSON: true})
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.Stdout.String())
	}
	var doc struct {
		Acts []struct {
			Steps []struct {
				Reads   []string `json:"reads"`
				Effects []string `json:"effects"`
			} `json:"steps"`
		} `json:"acts"`
	}
	if err := json.Unmarshal([]byte(h.Stdout.String()), &doc); err != nil {
		t.Fatalf("%v", err)
	}
	reads := doc.Acts[0].Steps[0].Reads
	if len(reads) != 2 || reads[0] != "read: ./in.json" || reads[1] != "fetch: https://api.example.com/x" {
		t.Errorf("reads: %v", reads)
	}
	if len(doc.Acts[0].Steps[0].Effects) != 0 {
		t.Errorf("reads must not appear as wreckage: %v", doc.Acts[0].Steps[0].Effects)
	}
}

// --- D-060: bindings reach the shell as environment --------------------------

func TestRunWithBindingsBecomeEnvironment(t *testing.T) {
	h := host.NewFake()
	// D-065: the Fake keys on the RAW command and records env as data — a
	// test asserts the map and the command separately, so it cannot plant
	// the string it asserts (the U8 false-pass, made structurally impossible).
	h.Shells["echo hello-$WHO"] = host.Shell{Stdout: "hello-hermes\n"}
	code := RunWith("@who = \"hermes\"\n@out = run echo hello-$WHO\n       with { WHO: @who }\nsend @out to output", h, Options{})
	if code != ExitOK {
		t.Fatalf("exit %d\nstderr: %s", code, h.Stderr.String())
	}
	if len(h.Ran) != 1 || h.Ran[0].Command != "echo hello-$WHO" {
		t.Fatalf("command must stay unspliced: %+v", h.Ran)
	}
	if h.Ran[0].Env["WHO"] != "hermes" {
		t.Errorf("env: %+v", h.Ran[0].Env)
	}
}

func TestRunWithRejectsBadEnvNames(t *testing.T) {
	h := host.NewFake()
	h.Shells["emit env"] = host.Shell{Stdout: `{"BAD-KEY": "x"}` + "\n"}
	code := RunWith("@env = run emit env\n@rec = @env |> parse\n@out = run echo x\n       with @rec\nsend @out to output", h, Options{})
	if code != ExitRuntime {
		t.Fatalf("exit %d — a hyphenated env name must fail\nstderr: %s", code, h.Stderr.String())
	}
	if !strings.Contains(h.Stderr.String(), "environment name") {
		t.Errorf("stderr: %s", h.Stderr.String())
	}
}

// --- D-061/D-062: parse and lines --------------------------------------------

func TestParseReentersTheValueWorld(t *testing.T) {
	h := host.NewFake()
	h.Shells["emit json"] = host.Shell{Stdout: `{"files": ["a.mana", "b.mana"]}` + "\n"}
	code := RunWith("@raw = run emit json\n@v = @raw |> parse\n@n = @v.files |> count\nsend @n to output", h, Options{})
	if code != ExitOK {
		t.Fatalf("exit %d\nstderr: %s", code, h.Stderr.String())
	}
	if strings.TrimSpace(h.Stdout.String()) != "2" {
		t.Errorf("got %q", h.Stdout.String())
	}
}

func TestParseFailsHardOnProse(t *testing.T) {
	h := host.NewFake()
	code := RunWith("@v = \"not json at all\" |> parse\nsend @v to output", h, Options{})
	if code != ExitRuntime {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.Stderr.String(), "not valid JSON") {
		t.Errorf("stderr: %s", h.Stderr.String())
	}
}

func TestLinesSplitsShellOutput(t *testing.T) {
	h := host.NewFake()
	h.Shells["ls examples"] = host.Shell{Stdout: "a.mana\nb.mana\nc.txt\n"}
	code := RunWith("@files = run ls examples\n@mana = @files |> lines |> filter where (@ |> matches \".mana\")\n@n = @mana |> count\nsend @n to output", h, Options{})
	if code != ExitOK {
		t.Fatalf("exit %d\nstderr: %s", code, h.Stderr.String())
	}
	if strings.TrimSpace(h.Stdout.String()) != "2" {
		t.Errorf("got %q", h.Stdout.String())
	}
}

// --- D-063: a recovered step says so ------------------------------------------

func TestRecoveredStepSaysSo(t *testing.T) {
	h := host.NewFake()
	h.Files["./cache.json"] = `{"cached": true}`
	code := RunWith("-- live if possible, cache if not\n@a = read ./absent.json\n     or read ./cache.json\nsend @a to output", h, Options{JSON: true})
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.Stdout.String())
	}
	var doc struct {
		Acts []struct {
			Steps []struct {
				Status string   `json:"status"`
				Notes  []string `json:"notes"`
			} `json:"steps"`
		} `json:"acts"`
	}
	if err := json.Unmarshal([]byte(h.Stdout.String()), &doc); err != nil {
		t.Fatal(err)
	}
	step := doc.Acts[0].Steps[0]
	if step.Status != "recovered" {
		t.Errorf("status %q, want recovered", step.Status)
	}
	if len(step.Notes) == 0 || !strings.Contains(step.Notes[0], "recovered by or") {
		t.Errorf("notes: %v", step.Notes)
	}
}

func TestAllOptionsFailingStaysFailed(t *testing.T) {
	h := host.NewFake()
	code := RunWith("-- everything is broken\n@a = read ./absent-1.json\n     or read ./absent-2.json\nsend @a to output", h, Options{JSON: true})
	if code != ExitRuntime {
		t.Fatalf("exit %d", code)
	}
	var doc struct {
		Acts []struct {
			Steps []struct {
				Status string `json:"status"`
			} `json:"steps"`
		} `json:"acts"`
	}
	json.Unmarshal([]byte(h.Stdout.String()), &doc)
	if doc.Acts[0].Steps[0].Status != "failed" {
		t.Errorf("status %q, want failed — no option succeeded", doc.Acts[0].Steps[0].Status)
	}
}

func TestCatchingAnEarlierStepsFailureStaysOk(t *testing.T) {
	h := host.NewFake()
	code := RunWith("-- bind a failure\n@bad = read ./absent.json\n\n-- handle it here\n@v = @bad or \"default\"\nsend @v to output", h, Options{JSON: true})
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.Stdout.String())
	}
	var doc struct {
		Acts []struct {
			Steps []struct {
				Intent string `json:"intent"`
				Status string `json:"status"`
			} `json:"steps"`
		} `json:"acts"`
	}
	json.Unmarshal([]byte(h.Stdout.String()), &doc)
	steps := doc.Acts[0].Steps
	if steps[0].Status != "failed" || steps[1].Status != "ok" {
		t.Errorf("want failed then ok (recovery is about this step's own attempt): %+v", steps)
	}
}

func TestTimeoutFlagReachesTheHost(t *testing.T) {
	h := host.NewFake()
	h.Shells["quick"] = host.Shell{Stdout: "ok"}
	RunWith("@x = run quick", h, Options{Timeout: 7 * time.Second})
	if len(h.Ran) != 1 || h.Ran[0].Timeout != 7*time.Second {
		t.Fatalf("got %+v", h.Ran)
	}
}

// Hermes P1: a paste that dies inside an open block must not look like a
// clean exit — buffered input that never ran is loss, and loss is reported.
func TestReplReportsInputEndingInsideAnOpenBlock(t *testing.T) {
	h := host.NewFake()
	err := Start(strings.NewReader("@x = {\n    server: 1\n"), h)
	if err == nil {
		t.Fatal("EOF inside an open block returned a clean exit")
	}
	if !strings.Contains(h.Stderr.String(), "open block") {
		t.Errorf("stderr: %q", h.Stderr.String())
	}
}
