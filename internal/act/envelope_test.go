package act

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/typedmirror/mana/internal/host"
	"github.com/typedmirror/mana/internal/object"
)

// declaring is a test module with a declared footprint (D-056).
type declaredModule struct {
	name    string
	effects []string
}

func (m declaredModule) Name() string                   { return m.name }
func (m declaredModule) Clauses() []string              { return nil }
func (m declaredModule) Execute(host.Call) object.Value { return object.Null{} }
func (m declaredModule) Effects() []string              { return m.effects }

func emit(t *testing.T, h *host.Fake, src string) []ActEnvelope {
	t.Helper()
	prog := parseProg(t, src)
	if h == nil {
		h = host.NewFake()
	}
	blob, err := Envelopes(prog, h)
	if err != nil {
		t.Fatalf("emit failed: %s", err.Inspect())
	}
	var fam struct {
		Acts []ActEnvelope `json:"acts"`
	}
	if jerr := json.Unmarshal(blob, &fam); jerr != nil {
		t.Fatalf("emission is not valid JSON: %v\n%s", jerr, blob)
	}
	return fam.Acts
}

func one(t *testing.T, acts []ActEnvelope, name string) ActEnvelope {
	t.Helper()
	for _, a := range acts {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("no envelope for act %q", name)
	return ActEnvelope{}
}

// A raw shell line is ⊤ on subprocess by ruling (D-052), and the emission
// says so by name rather than guessing basenames.
func TestRunProjectsToTopHonestly(t *testing.T) {
	acts := emit(t, nil, `act "shell" {
    @a = run git status
    send @a
}`)
	a := one(t, acts, "shell")
	if len(a.Envelope.Subprocess) != 1 || a.Envelope.Subprocess[0] != "*" {
		t.Errorf("subprocess: %v", a.Envelope.Subprocess)
	}
	if len(a.Top) != 1 || a.Top[0] != "run" {
		t.Errorf("the ⊤ must be attributed: %v", a.Top)
	}
	if a.EffectLevel != "io" {
		t.Errorf("level: %s", a.EffectLevel)
	}
}

// An undeclared module projects to ⊤ on every axis — the honest default.
func TestUndeclaredModuleIsTopEverywhere(t *testing.T) {
	h := host.NewFake()
	h.Register("mystery", func(host.Call) object.Value { return object.Null{} })
	acts := emit(t, h, `act "uses-mystery" {
    use mystery
    send 1
}`)
	a := one(t, acts, "uses-mystery")
	if !a.Envelope.Network || len(a.Envelope.Subprocess) == 0 || len(a.Envelope.FSRead) == 0 || len(a.Envelope.FSWrite) == 0 {
		t.Errorf("undeclared module must widen every axis: %+v", a.Envelope)
	}
	if len(a.Modules) != 1 || a.Modules[0].Declared {
		t.Errorf("modules: %+v", a.Modules)
	}
	if len(a.Top) == 0 || !strings.Contains(a.Top[0], "mystery") {
		t.Errorf("the ⊤ must be attributed: %v", a.Top)
	}
}

// A declared module contributes exactly its declaration.
func TestDeclaredModuleContributesItsFootprint(t *testing.T) {
	h := host.NewFake()
	h.Mods["notify"] = declaredModule{name: "notify", effects: []string{"network"}}
	acts := emit(t, h, `act "notifies" {
    use notify
    send 1
}`)
	a := one(t, acts, "notifies")
	if !a.Envelope.Network {
		t.Error("declared network effect missing")
	}
	if len(a.Envelope.Subprocess) != 0 || len(a.Envelope.FSWrite) != 0 {
		t.Errorf("declared module must not widen undeclared axes: %+v", a.Envelope)
	}
	if len(a.Top) != 0 {
		t.Errorf("nothing forced ⊤ here: %v", a.Top)
	}
}

// Path literals are carried as written; computed paths are "*".
func TestPathsAsWrittenComputedAsStar(t *testing.T) {
	acts := emit(t, nil, `act "files" {
    @cfg = read ./config.json
    @p = "./out/" + "result.json"
    @data = read @p
    @w = write @cfg to ./copy.json
    send @w
}`)
	a := one(t, acts, "files")
	joined := strings.Join(a.Envelope.FSRead, " ")
	if !strings.Contains(joined, "./config.json") || !strings.Contains(joined, "*") {
		t.Errorf("fs_read: %v", a.Envelope.FSRead)
	}
	if len(a.Envelope.FSWrite) != 1 || a.Envelope.FSWrite[0] != "./copy.json" {
		t.Errorf("fs_write: %v", a.Envelope.FSWrite)
	}
}

// The effect levels align with the two-phase gate: reads observe, mutations
// are io, and a computation touching nothing is pure.
func TestEffectLevels(t *testing.T) {
	acts := emit(t, nil, `act "pure" {
    send [1, 2, 3] |> sum
}

act "observer" {
    @a = read ./data.json
    send @a
}

act "actor" depends on "observer" {
    @w = write act.observer.result to ./out.json
    send @w
}`)
	for name, want := range map[string]string{"pure": "pure", "observer": "observe", "actor": "io"} {
		if got := one(t, acts, name).EffectLevel; got != want {
			t.Errorf("%s: %s, want %s", name, got, want)
		}
	}
}

// fetch and an http send are network; a send to output is nothing.
func TestNetworkAxes(t *testing.T) {
	acts := emit(t, nil, `act "wire" {
    @a = fetch https://api.example.com/health
    send @a to https://hooks.example.com/notify
    send "done" to output
}`)
	a := one(t, acts, "wire")
	if !a.Envelope.Network {
		t.Error("network must be true")
	}
	if len(a.Envelope.Subprocess) != 0 || len(a.Envelope.FSWrite) != 0 {
		t.Errorf("nothing else should widen: %+v", a.Envelope)
	}
}

// An effect buried in a match arm is still found — the walker sees branches.
func TestEffectInsideAMatchArmIsFound(t *testing.T) {
	acts := emit(t, nil, `act "branchy" {
    @r = read ./maybe.json
    @out = @r |> match {
        ok v: v
        err _: run echo fallback
    }
    send @out
}`)
	a := one(t, acts, "branchy")
	if len(a.Envelope.Subprocess) != 1 {
		t.Errorf("the run inside the err arm must be seen: %+v", a.Envelope)
	}
}

// A flat script is the unnamed act, one envelope.
func TestFlatScriptIsOneEnvelope(t *testing.T) {
	acts := emit(t, nil, `@a = read ./x.json
send @a to output`)
	if len(acts) != 1 || acts[0].Name != "" {
		t.Fatalf("got %+v", acts)
	}
	if acts[0].EffectLevel != "observe" {
		t.Errorf("level: %s", acts[0].EffectLevel)
	}
}

// The emission validates the graph the way the job would: an unrunnable
// script gets no envelope, it gets the error.
func TestBrokenGraphEmitsNothing(t *testing.T) {
	prog := parseProg(t, `act "a" depends on "b" { send 1 }
act "b" depends on "a" { send 2 }`)
	_, err := Envelopes(prog, host.NewFake())
	if err == nil || !strings.Contains(err.Reason, "cycle") {
		t.Fatalf("got %+v", err)
	}
}

// Hermes F1: a mixed flat+act script can never run, so it must get the
// runtime's own rejection from the emission — not a plausible-looking
// family with the loose statements invisible.
func TestMixedScriptEmitsTheRuntimeError(t *testing.T) {
	prog := parseProg(t, `write "loose" to ./mixed.txt
act "a" { send 1 }`)
	_, err := Envelopes(prog, host.NewFake())
	if err == nil || !strings.Contains(err.Reason, "either flat or made of acts") {
		t.Fatalf("emission accepted an unrunnable script: %+v", err)
	}
}

// Hermes F1, same hole in the dry run: planning a script the runtime
// rejects describes a run that will never happen.
func TestMixedScriptGetsNoDryRunPlan(t *testing.T) {
	prog := parseProg(t, `@x = 1
act "a" { send @x }`)
	_, err := DryRun(prog, host.NewFake())
	if err == nil || !strings.Contains(err.Reason, "either flat or made of acts") {
		t.Fatalf("dry run accepted an unrunnable script: %+v", err)
	}
}

// A file-level `--` line is reasoning about the job as a whole and stays
// legal alongside acts — only effectful loose statements are the defect.
func TestFileLevelIntentIsStillLegal(t *testing.T) {
	acts := emit(t, nil, `-- the job as a whole
act "a" { send 1 }`)
	if len(acts) != 1 || acts[0].Name != "a" {
		t.Fatalf("got %+v", acts)
	}
}

// Hermes F2: a declaration carrying an unknown effect token cannot be
// trusted, so it widens to ⊤ with the cause named — it must never silently
// narrow to nothing while the module still runs.
func TestInvalidEffectTokenWidensToTop(t *testing.T) {
	h := host.NewFake()
	h.Mods["stub"] = declaredModule{name: "stub", effects: []string{"netwrok"}}
	acts := emit(t, h, `act "x" {
    use stub
    send 1
}`)
	a := one(t, acts, "x")
	if !a.Envelope.Network || len(a.Envelope.Subprocess) == 0 || len(a.Envelope.FSRead) == 0 || len(a.Envelope.FSWrite) == 0 {
		t.Errorf("an untrustworthy declaration must widen every axis: %+v", a.Envelope)
	}
	if len(a.Top) == 0 || !strings.Contains(strings.Join(a.Top, " "), "netwrok") {
		t.Errorf("the bad token must be named in top: %v", a.Top)
	}
	if len(a.Modules) != 1 || len(a.Modules[0].Effects) != 1 || a.Modules[0].Effects[0] != "netwrok" {
		t.Errorf("the declaration stays visible as given: %+v", a.Modules)
	}
}
