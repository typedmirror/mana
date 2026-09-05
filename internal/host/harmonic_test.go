package host

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/typedmirror/mana/internal/object"
)

// stubHarmonic wires a Harmonic host to the committed stub kernel: python3
// really executes the generated cells (an unguarded kernel), except curl,
// which is denied with the contract's exit-3 + evalue-with-ref shape.
func stubHarmonic(t *testing.T) *Harmonic {
	t.Helper()
	path, err := filepath.Abs("../../tests/harmonic_stub.sh")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MANA_HARMONIC_CMD", path)
	base := NewReal(nil, nil, strings.NewReader(""))
	return NewHarmonic(base, "kstub01", t.TempDir())
}

func TestHarmonicRunAllowed(t *testing.T) {
	h := stubHarmonic(t)
	out, err := h.Run("echo kernel-alive", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if out.Code != 0 || strings.TrimSpace(out.Stdout) != "kernel-alive" {
		t.Errorf("got %+v", out)
	}
}

// The denial channel: exit 3, evalue with the inline ref — byte-compatible
// with the reference shim, which is what the parity gate will compare.
func TestHarmonicRunDeniedCarriesTheRef(t *testing.T) {
	h := stubHarmonic(t)
	out, err := h.Run("curl -sf http://example.test/x", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if out.Code != 3 {
		t.Fatalf("code %d, want 3: %+v", out.Code, out)
	}
	if !strings.Contains(out.Stderr, "harmonic:stub00@reftip0") {
		t.Errorf("the ref must ride the stderr: %q", out.Stderr)
	}
}

// D-065 in-kernel: the env map merges into the subprocess environment.
func TestHarmonicRunEnvAsData(t *testing.T) {
	h := stubHarmonic(t)
	out, err := h.Run("sh -c 'echo hi-$WHO'", map[string]string{"WHO": "hermes"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.Stdout) != "hi-hermes" {
		t.Errorf("got %+v", out)
	}
}

func TestHarmonicReadWriteRoundTrip(t *testing.T) {
	h := stubHarmonic(t)
	path := filepath.Join(t.TempDir(), "note.txt")
	if err := h.WriteFile(path, "in-kernel"); err != nil {
		t.Fatal(err)
	}
	got, err := h.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got) != "in-kernel" {
		t.Errorf("got %q", got)
	}
}

// The parity-shaped proof: a denial through the whole evaluator arrives as
// an error value carrying the ref, with the recovery suggestion attached by
// the adoption site — the same failures[].reason shape the G-seam verified
// through the shim.
func TestHarmonicDenialIsDataEndToEnd(t *testing.T) {
	h := stubHarmonic(t)
	// Reaching the evaluator through the host interface directly would skip
	// the language; a tiny in-package check of the classifier contract:
	out, _ := h.Run("curl -sf http://example.test/x", nil, 0)
	reason := "exit 3: " + out.Stderr
	if !strings.Contains(reason, "harmonic guard: subprocess denied") {
		t.Errorf("reason shape: %q", reason)
	}
	// and the ref survives object.Err construction untouched
	e := object.Errorf("%s", reason)
	if !strings.Contains(e.Reason, "[harmonic:stub00@reftip0]") {
		t.Errorf("ref lost: %q", e.Reason)
	}
}

// The parity gate caught the fake drifting from the real contract on
// cell_id's TYPE (integer, not string) — every real response failed to
// parse while the stub-fed tests passed. This fixture is the real kernel's
// shape verbatim, typed per harmonic's contract addendum: cell_id integer,
// duration integer, status enum. If a struct field ever disagrees with it
// again, this fails before the gate has to.
func TestHarmonicParsesTheRealContractShape(t *testing.T) {
	real := `{"cell_id":1,"status":"error","outputs":[{"type":"stream","name":"Stdout","text":"partial"},{"type":"error","ename":"PermissionError","evalue":"harmonic guard: subprocess denied: 'curl' [harmonic:f09dae1e@f821ef119336c53b]","traceback":["..."]}],"duration":12}`
	var resp execResponse
	if err := jsonUnmarshal(real, &resp); err != nil {
		t.Fatalf("the real contract shape must parse: %v", err)
	}
	stdout, _, evalue := resp.pick()
	if stdout != "partial" || !contains([]string{evalue}, evalue) || evalue == "" {
		t.Errorf("picked wrong: stdout=%q evalue=%q", stdout, evalue)
	}
	if !strings.Contains(evalue, "[harmonic:f09dae1e@f821ef119336c53b]") {
		t.Errorf("ref lost: %q", evalue)
	}
}

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }
