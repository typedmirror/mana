package host

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/typedmirror/mana/internal/object"
)

// Claude delegates to a model through the claude CLI (D-040, D-041). A
// subagent is consultation: the prompt goes in on stdin, the reply comes back
// as a value, and nothing the subagent says is ever executed (D-042).
//
// The caller's `--` line rides into the subagent as a system-prompt addition
// (D-043) — the intent channel does not terminate at a process boundary.
type Claude struct {
	cmd     string
	timeout time.Duration
}

// NewClaude resolves the backend command once, at construction. MANA_CLAUDE_CMD
// overrides the default for stubbing (D-047); a value containing a path
// separator is made absolute here because the subprocess runs in a different
// directory and would resolve a relative path against the wrong root.
func NewClaude() *Claude {
	cmd := os.Getenv("MANA_CLAUDE_CMD")
	if cmd == "" {
		cmd = "claude"
	}
	if strings.ContainsRune(cmd, filepath.Separator) {
		if abs, err := filepath.Abs(cmd); err == nil {
			cmd = abs
		}
	}
	return &Claude{cmd: cmd, timeout: DefaultTimeout}
}

func (c *Claude) Name() string { return "claude" }

// Clauses is nil: the module speaks only the built-in keywords. `with` carries
// the model choice and any context values; `as` picks the return shape.
func (c *Claude) Clauses() []string { return nil }

func (c *Claude) Execute(call Call) object.Value {
	if call.Target != "" {
		bad := Fail("claude takes a prompt string, not a target %q", call.Target)
		bad.Suggestion = `write claude "…prompt…", with the prompt as a string`
		return bad
	}
	if len(call.Args) == 0 {
		bad := Fail("claude needs a prompt")
		bad.Suggestion = `write claude "…prompt…", with the prompt as a string`
		return bad
	}
	if _, ok := call.Args[0].(object.String); !ok {
		bad := Fail("claude needs its prompt as a string, got %s", call.Args[0].Type())
		bad.Suggestion = `write claude "…prompt…", with the prompt as a string`
		return bad
	}

	format, bad := c.format(call)
	if bad != nil {
		return bad
	}
	model, prompt := c.assemble(call)

	argv := []string{
		"-p",
		"--output-format", "text",
		"--safe-mode",
		"--tools", "",
		"--no-session-persistence",
	}
	if model != "" {
		argv = append(argv, "--model", model)
	}
	if call.Intent != "" {
		argv = append(argv, "--append-system-prompt", "Caller intent: "+call.Intent)
	}

	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.cmd, argv...)
	// The subagent must not inherit the calling project's context: --safe-mode
	// skips CLAUDE.md auto-discovery, and running in the temp directory means
	// there is nothing to discover even if that flag's behaviour drifts (D-041).
	cmd.Dir = os.TempDir()
	cmd.Stdin = strings.NewReader(prompt)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	var stdout, stderr capped
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	if ctx.Err() == context.DeadlineExceeded {
		return Fail("subagent timed out after %s", c.timeout)
	}
	if err != nil {
		var exitErr *exec.ExitError
		if asExitError(err, &exitErr) {
			return Fail("subagent exited %d: %s", exitErr.ExitCode(), tail(stderr.String(), 240))
		}
		bad := Fail("cannot run %q: %v", c.cmd, err)
		bad.Suggestion = "install the claude CLI, or point MANA_CLAUDE_CMD at it"
		return bad
	}

	text := stdout.String()
	if strings.TrimSpace(text) == "" {
		// Exit 0 with nothing to show is not an answer (I1).
		return Fail("subagent returned nothing")
	}

	switch format {
	case "json":
		v, jerr := object.ParseJSON(strings.TrimSpace(text))
		if jerr != nil {
			return Fail("subagent reply is not the json that was asked for: %v", jerr)
		}
		return v
	default:
		return object.String(strings.TrimSuffix(text, "\n"))
	}
}

// format reads the `as` clause: text (the default) or json (D-044).
func (c *Claude) format(call Call) (string, *object.Err) {
	v, ok := call.Clauses["as"]
	if !ok {
		return "text", nil
	}
	f := strings.ToLower(word(v))
	if f != "text" && f != "json" {
		bad := Fail("claude cannot answer as %q", word(v))
		bad.Suggestion = "valid formats: text, json"
		return "", bad
	}
	return f, nil
}

// assemble builds the outgoing prompt: the string arguments joined, then each
// `with` value as a labelled block. `with model "…"` is lifted out as the
// model choice rather than sent as context.
func (c *Claude) assemble(call Call) (model, prompt string) {
	var b strings.Builder
	for i, a := range call.Args {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(rendered(a))
	}
	if v, ok := call.Clauses["with"]; ok {
		if rec, isRec := v.(*object.Record); isRec {
			for _, k := range rec.Keys() {
				val, _ := rec.Get(k)
				if k == "model" {
					model = word(val)
					continue
				}
				b.WriteString("\n\n")
				b.WriteString(k)
				b.WriteString(":\n")
				b.WriteString(rendered(val))
			}
		} else {
			b.WriteString("\n\ninput:\n")
			b.WriteString(rendered(v))
		}
	}
	return model, b.String()
}

// rendered turns a value into prompt text: strings go through as they are,
// everything else as JSON so the subagent sees structure, not Inspect noise.
func rendered(v object.Value) string {
	if s, ok := v.(object.String); ok {
		return string(s)
	}
	return object.JSON(v)
}

// word reads a Word or String clause value as its text.
func word(v object.Value) string {
	switch w := v.(type) {
	case object.Word:
		return string(w)
	case object.String:
		return string(w)
	default:
		return v.Inspect()
	}
}

// tail keeps the last n characters of a diagnostic, whitespace-trimmed — the
// reader is a context window, and stderr's end is where the reason lives.
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
