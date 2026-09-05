package host

import (
	"strings"
	"testing"
	"time"
)

// The real shell is the only honest instrument for expansion semantics —
// hermes U8 proved a Fake can false-pass them. These run the actual $SHELL.

// D-065: env realized as exports ahead of the line, so same-line $KEY
// expansion sees the value — the form the README advertises.
func TestRealRunEnvExpandsOnTheSameLine(t *testing.T) {
	h := NewReal(nil, nil, strings.NewReader(""))
	out, err := h.Run("echo hello-$WHO", map[string]string{"WHO": "hermes"}, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.Stdout) != "hello-hermes" {
		t.Errorf("stdout %q — expansion did not see the export", out.Stdout)
	}
}

// A hostile value stays a value: quoted once by the realization, never a
// second command.
func TestRealRunEnvQuotesHostileValues(t *testing.T) {
	h := NewReal(nil, nil, strings.NewReader(""))
	hostile := "a'; echo PWNED; echo 'b"
	out, err := h.Run(`printf %s "$V"`, map[string]string{"V": hostile}, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stdout != hostile {
		t.Errorf("value did not round-trip intact: %q", out.Stdout)
	}
	if strings.Contains(out.Stdout, "PWNED\n") || out.Code != 0 {
		t.Errorf("the value escaped its quoting: %+v", out)
	}
}

// Env order is deterministic so a command's realization can be goldened.
func TestRealRunEnvOrderIsDeterministic(t *testing.T) {
	h := NewReal(nil, nil, strings.NewReader(""))
	out, err := h.Run("echo $A-$B-$C", map[string]string{"C": "3", "A": "1", "B": "2"}, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.Stdout) != "1-2-3" {
		t.Errorf("got %q", out.Stdout)
	}
}
