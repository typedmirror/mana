package act

import (
	"bytes"
	"strings"
	"testing"

	"github.com/typedmirror/mana/internal/host"
)

// The D-066 acceptance jobs: a three-act chain where "a" provisions with a
// real (fake-ledgered) effect, "b" finishes on top of it, "c" summarizes.
// Run one fails at "b"; the resume must finish the job without provisioning
// twice.
const resumeSrc = `
act "a" {
  -- provision the thing
  @r = run provision
  send @r
}

act "b" depends on "a" {
  -- finish on top of a
  @r = run finish
  send @r
}

act "c" depends on "b" {
  -- summarize what b established
  send act.b.result
}
`

// firstRun executes the job against a world where "finish" does not exist
// yet, returning the failed run's JSON report — the resume token.
func firstRun(t *testing.T) []byte {
	t.Helper()
	f := host.NewFake()
	f.Shells["provision"] = host.Shell{Stdout: "made\n"}
	r, _ := runWithOpts(t, f, resumeSrc, Options{})
	if r.OK() {
		t.Fatalf("first run should fail at b")
	}
	if got := outcome(t, r, "a").Status; got != Succeeded {
		t.Fatalf("a: %s, want ok", got)
	}
	blob, err := JSON(r, "")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return blob
}

// The failure path first: a hand-edited report is refused loudly, never
// quietly obeyed and never quietly discarded (acceptance criterion 3).
func TestResumeRefusesTamperedReport(t *testing.T) {
	blob := firstRun(t)
	tampered := bytes.Replace(blob, []byte(`"made"`), []byte(`"fake"`), 1)
	if bytes.Equal(tampered, blob) {
		t.Fatalf("tamper did not land; fixture drifted")
	}
	if _, _, err := PriorFromReport(tampered); err == nil {
		t.Fatalf("tampered report was accepted")
	} else if !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("refusal does not name the reason: %v", err)
	}
}

// A report with no seal (predating resume, or stripped) is refused when it
// claims resumable successes.
func TestResumeRefusesUnsealedReport(t *testing.T) {
	blob := firstRun(t)
	stripped := bytes.Replace(blob, []byte(`"integrity"`), []byte(`"integrity_gone"`), 1)
	if _, _, err := PriorFromReport(stripped); err == nil {
		t.Fatalf("unsealed report was accepted")
	} else if !strings.Contains(err.Error(), "seal") {
		t.Fatalf("refusal does not name the reason: %v", err)
	}
}

// An edited act reuses nothing: identity is the text, and a change re-runs
// everything downstream of it (acceptance criterion 2).
func TestResumeReusesNothingWhenActEdited(t *testing.T) {
	blob := firstRun(t)
	prior, _, err := PriorFromReport(blob)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	edited := strings.Replace(resumeSrc, "run provision", "run provision --deluxe", 1)
	f := host.NewFake()
	f.Shells["provision --deluxe"] = host.Shell{Stdout: "made\n"}
	f.Shells["finish"] = host.Shell{Stdout: "done\n"}
	r, _ := runWithOpts(t, f, edited, Options{Prior: prior})
	if !r.OK() {
		t.Fatalf("edited resume failed: %+v", r)
	}
	if got := outcome(t, r, "a").Status; got != Succeeded {
		t.Fatalf("edited a should have run, got %s", got)
	}
	ran := 0
	for _, c := range f.Ran {
		if strings.HasPrefix(c.Command, "provision") {
			ran++
		}
	}
	if ran != 1 {
		t.Fatalf("edited a should provision exactly once, ran %d", ran)
	}
}

// Without --resume there is no reuse, ever: the assertion is explicit or
// absent (acceptance criterion 4).
func TestNoFlagNoReuse(t *testing.T) {
	firstRun(t)
	f := host.NewFake()
	f.Shells["provision"] = host.Shell{Stdout: "made\n"}
	f.Shells["finish"] = host.Shell{Stdout: "done\n"}
	r, _ := runWithOpts(t, f, resumeSrc, Options{})
	if !r.OK() {
		t.Fatalf("second fresh run failed: %+v", r)
	}
	if got := outcome(t, r, "a").Status; got != Succeeded {
		t.Fatalf("fresh run must execute a, got %s", got)
	}
}

// The happy path (acceptance criterion 1): resume finishes the job, "a" is
// reused with its effect fired exactly once across both invocations, and the
// resumed report seals its own resumable set for the next link in the chain.
func TestResumeFinishesWithoutRefiring(t *testing.T) {
	blob := firstRun(t)
	prior, seal, err := PriorFromReport(blob)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if seal == "" || seal == "none" {
		t.Fatalf("a report with a success must carry a real seal, got %q", seal)
	}

	f := host.NewFake()
	f.Shells["provision"] = host.Shell{Stdout: "made\n"}
	f.Shells["finish"] = host.Shell{Stdout: "done\n"}
	r, _ := runWithOpts(t, f, resumeSrc, Options{Prior: prior})
	if !r.OK() {
		t.Fatalf("resume failed: %+v", r)
	}
	if got := outcome(t, r, "a").Status; got != Reused {
		t.Fatalf("a: %s, want reused", got)
	}
	if got := outcome(t, r, "b").Status; got != Succeeded {
		t.Fatalf("b: %s, want ok", got)
	}
	if got := outcome(t, r, "c").Status; got != Succeeded {
		t.Fatalf("c: %s, want ok", got)
	}
	for _, c := range f.Ran {
		if c.Command == "provision" {
			t.Fatalf("reused act re-fired its effect")
		}
	}
	// c consumed the RESTORED result of the chain a→b.
	if got := outcome(t, r, "c").Result.Inspect(); !strings.Contains(got, "done") {
		t.Fatalf("c saw %q, want b's result", got)
	}

	// The resumed report is itself a valid resume token: lineage works.
	blob2, err := JSON(r, "")
	if err != nil {
		t.Fatalf("encode resumed report: %v", err)
	}
	if _, seal2, err := PriorFromReport(blob2); err != nil {
		t.Fatalf("resumed report does not load: %v", err)
	} else if seal2 == "" || seal2 == "none" {
		t.Fatalf("resumed report carries no seal")
	}
}

// An imported act's identity must include the resolved body, not just the
// `from` path. Editing the imported file must invalidate the cached result.
func TestResumeInvalidatesEditedImport(t *testing.T) {
	const src = `act "work" from ./worker.mana

act "report" depends on "work" {
  send act.work.result
}
`
	// First run: work succeeds (imported body runs "work" shell command).
	f := host.NewFake()
	f.Files["./worker.mana"] = "-- original\n@r = run work\nsend @r"
	f.Shells["work"] = host.Shell{Stdout: "v1\n"}
	r, _ := runWithOpts(t, f, src, Options{})
	if !r.OK() {
		t.Fatalf("first run failed: %+v", r)
	}
	blob, _ := JSON(r, "")
	prior, _, err := PriorFromReport(blob)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// Edit the imported file — the body changes but the path stays the same.
	f2 := host.NewFake()
	f2.Files["./worker.mana"] = "-- updated\n@r = run work-v2\nsend @r"
	f2.Shells["work-v2"] = host.Shell{Stdout: "v2\n"}
	r2, _ := runWithOpts(t, f2, src, Options{Prior: prior})
	if !r2.OK() {
		t.Fatalf("resumed run failed: %+v", r2)
	}
	if got := outcome(t, r2, "work").Status; got == Reused {
		t.Fatalf("imported act was wrongly reused after its file was edited")
	}
}
