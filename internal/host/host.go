// Package host is the I/O boundary.
//
// Every effect Mana can cause goes through this interface, which exists so the
// evaluator can be tested without a network, a filesystem, or a shell. Spec §5
// says every verb is effectful; this is the one place those effects live.
package host

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strings"
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

// Shell is the outcome of a `run`.
type Shell struct {
	Stdout string
	Stderr string
	Code   int
}

// Host is every side effect Mana can cause.
type Host interface {
	Fetch(url string) (string, error)
	ReadFile(path string) (string, error)
	WriteFile(path, content string) error
	Run(command string) (Shell, error)
	Post(url, body string) (string, error)
	Ask(prompt string) (string, error)

	// Tools are ambient (spec §11): the environment decides what exists, and a
	// script never imports one.
	ToolNames() []string
	CallTool(name string, args object.Value) (object.Value, error)

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

// Run executes a command through the user's shell. Spec §5.2: "The agent's bash
// is Mana's bash" — so no argument splitting and no sandbox, which is a
// deliberate capability, not an oversight.
func (h *Real) Run(command string) (Shell, error) {
	sh := os.Getenv("SHELL")
	if sh == "" {
		sh = "/bin/sh"
	}
	cmd := exec.Command(sh, "-c", command)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	out := Shell{Stdout: stdout.String(), Stderr: stderr.String()}
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

// ToolNames reports the ambient tools. The real host binds none: a script that
// asks for a tool gets an honest empty list rather than a plausible-looking one.
func (h *Real) ToolNames() []string { return nil }

func (h *Real) CallTool(name string, _ object.Value) (object.Value, error) {
	return nil, fmt.Errorf("no tool named %q is bound in this environment", name)
}

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
