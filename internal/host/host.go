// Package host is the I/O boundary.
//
// Every effect Mana can cause goes through this interface, which exists so the
// evaluator can be tested without a network, a filesystem, or a shell. Spec §5
// says every verb is effectful; this is the one place those effects live.
package host

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/typedmirror/mana/internal/object"
)

// Context is the ambient execution environment (spec §12). It is data, not a
// lookup, so a test can pin `today` and get a deterministic run.
type Context struct {
	User        string
	LastMessage string
	Messages    []string
	Cwd         string
	OS          string
	Today       string // date, e.g. 2026-08-07
	Now         string // timestamp, e.g. 2026-08-07T15:04:05Z
}

// Module is anything a script can reach through `use` (v2 §7.3).
//
// Clauses may return nil, which means the module accepts only the built-in
// clause keywords. That optionality is what lets a plain tool and a
// clause-declaring module share one interface without a second registry.
type Module interface {
	Name() string
	Clauses() []string
	Execute(Call) object.Value
}

// Call is one invocation of a module.
//
// SPEC NOTE: v2 §7.3 gives Execute the signature
// `(target string, clauses map[string]Value, intent string)`, which has nowhere
// to put a positional argument — but §7.1 writes `search_web "mana programming
// language"`, which is exactly that. Call carries Args as well; the spec
// signature cannot express its own example.
type Call struct {
	Target  string // the bare word after the module name, "" if there is none
	Args    []object.Value
	Clauses map[string]object.Value
	Intent  string // the `--` line in force, supplied automatically
}

// Fail is how a module reports a failure. Errors are data here as everywhere
// else (§12), so a module returns one rather than a Go error.
func Fail(format string, args ...any) *object.Err {
	return object.Errorf(format, args...)
}

// Func adapts a plain function to the Module interface. It declares no clauses,
// which is the honest answer for a tool that never had any.
type Func struct {
	ModuleName string
	Fn         func(Call) object.Value
}

func (m Func) Name() string                { return m.ModuleName }
func (m Func) Clauses() []string           { return nil }
func (m Func) Execute(c Call) object.Value { return m.Fn(c) }

// Shell is the outcome of a `run`.
type Shell struct {
	Stdout    string
	Stderr    string
	Code      int
	Truncated bool // output exceeded MaxOutputBytes and was cut
	TimedOut  bool // the command was killed at the deadline
}

// MaxOutputBytes bounds what a single command may return, per stream.
//
// Mana exists so a model can fire one artifact and read one result, which makes
// the result the model's context. An unbounded `run find /` would be a context
// bomb, so output is cut and the cut is stated rather than hidden.
const MaxOutputBytes = 32 << 10

// DefaultTimeout bounds a single command.
//
// Without one, `run npm start` never returns and the whole job hangs with
// nothing to show for it — the worst outcome for a caller that fired once and
// is waiting. Long-running work is backgrounded through the shell instead; see
// Run.
const DefaultTimeout = 2 * time.Minute

// capped collects output up to a limit and remembers whether it overflowed.
type capped struct {
	buf   []byte
	total int
}

func (c *capped) Write(p []byte) (int, error) {
	c.total += len(p)
	if room := MaxOutputBytes - len(c.buf); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		c.buf = append(c.buf, p[:room]...)
	}
	return len(p), nil
}

func (c *capped) String() string {
	if c.total <= len(c.buf) {
		return string(c.buf)
	}
	return fmt.Sprintf("%s\n… truncated, %d bytes total", c.buf, c.total)
}

func (c *capped) overflowed() bool { return c.total > len(c.buf) }

// Host is every side effect Mana can cause.
type Host interface {
	Fetch(url string) (string, error)
	ReadFile(path string) (string, error)
	WriteFile(path, content string) error
	Run(command string, timeout time.Duration) (Shell, error)
	Post(url, body string) (string, error)
	Ask(prompt string) (string, error)

	// Modules extend the verb system (v2 §7). The environment decides what
	// exists; a script reaches one with `use` and never imports it.
	//
	// Tools and modules are one registry, not two. Their shapes were already
	// almost identical — a name and something to execute — and two registries
	// would mean two lookup paths and two resolution rules for the same idea.
	Modules() []string
	Module(name string) (Module, bool)

	Context() Context

	// Out carries script output; Err carries the runtime's own commentary —
	// the intent channel echoed in the REPL, and failures. Keeping them apart
	// means `mana script.mana > out.json` yields only what the script sent.
	Out() io.Writer
	Err() io.Writer
}

// Real is the production Host.
type Real struct {
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader
	Client *http.Client
}

// NewReal returns a Host wired to the actual machine.
func NewReal(stdout, stderr io.Writer, stdin io.Reader) *Real {
	return &Real{
		Stdout: stdout,
		Stderr: stderr,
		Stdin:  stdin,
		Client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (h *Real) Out() io.Writer { return h.Stdout }

func (h *Real) Err() io.Writer { return h.Stderr }

func (h *Real) Fetch(url string) (string, error) {
	resp, err := h.Client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("%s returned %s", url, resp.Status)
	}
	return string(body), nil
}

func (h *Real) Post(url, body string) (string, error) {
	resp, err := h.Client.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("%s returned %s", url, resp.Status)
	}
	return string(out), nil
}

func (h *Real) ReadFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}

func (h *Real) WriteFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

// Run executes a command through the user's shell. Spec §6.2: "The agent's bash
// is Mana's bash" — so no argument splitting and no sandbox, which is a
// deliberate capability, not an oversight.
//
// Two bounds are not negotiable. A command is killed at the deadline, and each
// stream is capped. Both exist because the caller is usually a model that fired
// once and is waiting: a hang gives it nothing, and a gigabyte gives it too
// much.
//
// A command that should outlive the run is backgrounded through the shell —
// `run ./server & disown` — which needs no language feature because the shell
// already has one. Redirect its streams (`> /dev/null 2>&1`) or the pipe stays
// open and the deadline applies after all.
func (h *Real) Run(command string, timeout time.Duration) (Shell, error) {
	sh := os.Getenv("SHELL")
	if sh == "" {
		sh = "/bin/sh"
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, sh, "-c", command)
	// Kill the whole process group: a shell that spawned children and died
	// would otherwise leave them holding the pipes.
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

	out := Shell{
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		Truncated: stdout.overflowed() || stderr.overflowed(),
	}
	if ctx.Err() == context.DeadlineExceeded {
		out.TimedOut = true
		out.Code = -1
		return out, nil
	}
	var exitErr *exec.ExitError
	if err != nil {
		if ok := asExitError(err, &exitErr); ok {
			out.Code = exitErr.ExitCode()
			return out, nil // a non-zero exit is a result, not a host failure
		}
		return out, err
	}
	return out, nil
}

func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

func (h *Real) Ask(prompt string) (string, error) {
	fmt.Fprintf(h.Stdout, "%s ", prompt)
	scanner := bufio.NewScanner(h.Stdin)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", err
		}
		return "", fmt.Errorf("no answer available on stdin")
	}
	return strings.TrimSpace(scanner.Text()), nil
}

// Modules reports what this environment provides. The real host binds nothing
// yet: a script asking for a module gets an honest empty list rather than a
// plausible-looking one.
func (h *Real) Modules() []string { return nil }

func (h *Real) Module(string) (Module, bool) { return nil, false }

func (h *Real) Context() Context {
	now := time.Now().UTC()
	c := Context{
		OS:    runtime.GOOS,
		Today: now.Format("2006-01-02"),
		Now:   now.Format(time.RFC3339),
	}
	if cwd, err := os.Getwd(); err == nil {
		c.Cwd = cwd
	}
	if u, err := user.Current(); err == nil {
		c.User = u.Username
	}
	return c
}

// Capture wraps a Host and diverts script output into a buffer.
//
// It exists for --json, where the report is the answer: the script's own output
// belongs inside that document rather than interleaved with it, or a caller
// parsing the result has to separate two things that arrived on one stream.
type Capture struct {
	Host
	buf strings.Builder
}

func NewCapture(h Host) *Capture { return &Capture{Host: h} }

func (c *Capture) Out() io.Writer { return &c.buf }

// Text is everything the script sent to output.
func (c *Capture) Text() string { return c.buf.String() }
