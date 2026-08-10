package host

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/typedmirror/mana/internal/object"
)

// Fake is a scripted Host for tests. Every effect is recorded rather than
// caused, so a test can assert on what a script *did*, not only on what it
// returned — which is the half of a verb that matters.
//
// Acts with no edges between them run on concurrent goroutines against one
// shared host, so every method that touches shared state takes the mutex.
// Reads of the recorder fields are safe once the run has returned — the
// scheduler joins its goroutines before handing back the report.
type Fake struct {
	Responses map[string]string // url -> body
	Files     map[string]string // path -> contents
	Shells    map[string]Shell  // command -> outcome
	Answers   []string          // consumed by Ask, in order
	Mods      map[string]Module // registry, shared by tools and modules alike
	Ctx       Context

	Ran     []Ran     // every shell command, in order
	Written []Written // every WriteFile, in order
	Posted  []Posted  // every Post, in order
	Asked   []string  // every prompt, in order
	Stdout  strings.Builder
	Stderr  strings.Builder

	mu sync.Mutex
}

type Written struct{ Path, Content string }

type Ran struct {
	Command string
	Timeout time.Duration
}

type Posted struct{ URL, Body string }

// NewFake returns a Fake with a pinned context, so `context.env.today` is
// deterministic and a test that formats a date does not fail at midnight.
func NewFake() *Fake {
	return &Fake{
		Responses: map[string]string{},
		Files:     map[string]string{},
		Shells:    map[string]Shell{},
		Mods:      map[string]Module{},
		Ctx: Context{
			User:        "tester",
			LastMessage: "run the report",
			Messages:    []string{"hello", "run the report"},
			Cwd:         "/work",
			OS:          "testos",
			Today:       "2026-08-07",
			Now:         "2026-08-07T09:00:00Z",
		},
	}
}

// lockedWriter serializes writes from concurrent acts into one builder.
type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (h *Fake) Out() io.Writer { return lockedWriter{mu: &h.mu, w: &h.Stdout} }

func (h *Fake) Err() io.Writer { return lockedWriter{mu: &h.mu, w: &h.Stderr} }

func (h *Fake) Context() Context { return h.Ctx }

func (h *Fake) Fetch(url string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	body, ok := h.Responses[url]
	if !ok {
		return "", fmt.Errorf("connection refused")
	}
	return body, nil
}

func (h *Fake) Post(url, body string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.Responses[url]; !ok {
		return "", fmt.Errorf("connection refused")
	}
	h.Posted = append(h.Posted, Posted{URL: url, Body: body})
	return h.Responses[url], nil
}

func (h *Fake) ReadFile(path string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	body, ok := h.Files[path]
	if !ok {
		return "", fmt.Errorf("no such file: %s", path)
	}
	return body, nil
}

func (h *Fake) WriteFile(path, content string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Written = append(h.Written, Written{Path: path, Content: content})
	h.Files[path] = content
	return nil
}

func (h *Fake) Run(command string, timeout time.Duration) (Shell, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Ran = append(h.Ran, Ran{Command: command, Timeout: timeout})
	out, ok := h.Shells[command]
	if !ok {
		return Shell{Code: 127, Stderr: "command not found"}, nil
	}
	return out, nil
}

func (h *Fake) Ask(prompt string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Asked = append(h.Asked, prompt)
	if len(h.Answers) == 0 {
		return "", fmt.Errorf("no answer available on stdin")
	}
	answer := h.Answers[0]
	h.Answers = h.Answers[1:]
	return answer, nil
}

// Modules returns the registry in sorted order. Map iteration order would make
// `ask tools` produce a different list every run, and a test that passes four
// times in five is worse than no test.
func (h *Fake) Modules() []string {
	names := make([]string, 0, len(h.Mods))
	for name := range h.Mods {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (h *Fake) Module(name string) (Module, bool) {
	m, ok := h.Mods[name]
	return m, ok
}

// Register binds a module with no custom clauses — the shape a plain tool has.
func (h *Fake) Register(name string, fn func(Call) object.Value) {
	h.Mods[name] = Func{ModuleName: name, Fn: fn}
}

// RegisterWithClauses binds a module that declares its own clause keywords.
func (h *Fake) RegisterWithClauses(name string, clauses []string, fn func(Call) object.Value) {
	h.Mods[name] = declaring{name: name, clauses: clauses, fn: fn}
}

// Calls records every module invocation, so a test can assert on what a script
// asked a module to do rather than only on what came back.
type Recorded struct {
	Module string
	Call   Call
}

type declaring struct {
	name    string
	clauses []string
	fn      func(Call) object.Value
}

func (m declaring) Name() string                { return m.name }
func (m declaring) Clauses() []string           { return m.clauses }
func (m declaring) Execute(c Call) object.Value { return m.fn(c) }
