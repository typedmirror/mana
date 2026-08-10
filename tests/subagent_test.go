package tests

import (
	"os"
	"strings"
	"testing"

	"github.com/typedmirror/mana/internal/host"
	"github.com/typedmirror/mana/internal/repl"
)

// subagentHost mirrors the binary's wiring (D-046): a Real host with the
// claude module registered, pointed at the committed stub (D-047). The
// override uses a ./-relative path on purpose — it proves the module's
// absolute-path resolution, since the subprocess runs in a different
// directory.
func subagentHost(t *testing.T, stdout, stderr *strings.Builder) *host.Real {
	t.Helper()
	t.Setenv("MANA_CLAUDE_CMD", "./claude_stub.sh")
	h := host.NewReal(stdout, stderr, strings.NewReader(""))
	h.Register(host.NewClaude())
	return h
}

// TestSubagentPanel is the fan-out: three acts consult concurrent subagents,
// a fourth joins and rules. The whole Real pathway runs — argv, stdin
// delivery, exit codes, JSON decode — against the stub.
func TestSubagentPanel(t *testing.T) {
	src, err := os.ReadFile("../examples/panel.mana")
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	h := subagentHost(t, &stdout, &stderr)
	if code := repl.Run(string(src), h); code != repl.ExitOK {
		t.Fatalf("exit %d\nstderr:\n%s", code, stderr.String())
	}
	compareGolden(t, "../examples/panel.expected", stdout.String())
}

// TestSubagentSkip is the failure half: a refusing subagent fails its act
// with the intent attached, and the dependent is skipped — not passed.
func TestSubagentSkip(t *testing.T) {
	src, err := os.ReadFile("subagent_skip.mana")
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	h := subagentHost(t, &stdout, &stderr)
	if code := repl.Run(string(src), h); code != repl.ExitRuntime {
		t.Fatalf("exit %d, want %d\nstderr:\n%s", code, repl.ExitRuntime, stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("skipped act leaked output: %q", stdout.String())
	}
	for _, want := range []string{
		`intent: "asking the oracle, which will refuse"`,
		`reason: "subagent exited 1: stub: refusing as instructed"`,
		`act publish: skipped — dependency "consult" failed`,
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("diagnostics missing %q:\n%s", want, stderr.String())
		}
	}
}

// TestSubagentNotUsedIsOutOfReach: registration is not permission. An act
// that never said `use claude` cannot reach the module, even though the host
// binds it (D-040, D-027).
func TestSubagentNotUsedIsOutOfReach(t *testing.T) {
	var stdout, stderr strings.Builder
	h := subagentHost(t, &stdout, &stderr)
	code := repl.Run(`act "sneaky" {
    -- reaching for a module this act never asked for
    @a = claude "hello"
    send @a
}`, h)
	if code != repl.ExitRuntime {
		t.Fatalf("exit %d, want %d\nstderr:\n%s", code, repl.ExitRuntime, stderr.String())
	}
	if !strings.Contains(stderr.String(), "use") {
		t.Errorf("error should point at the missing use:\n%s", stderr.String())
	}
}
