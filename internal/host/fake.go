package host

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/typedmirror/mana/internal/object"
)

// Fake is a scripted Host for tests. Every effect is recorded rather than
// caused, so a test can assert on what a script *did*, not only on what it
// returned — which is the half of a verb that matters.
type Fake struct {
	Responses map[string]string // url -> body
	Files     map[string]string // path -> contents
	Shells    map[string]Shell  // command -> outcome
	Answers   []string          // consumed by Ask, in order
	Tools     map[string]func(object.Value) (object.Value, error)
	Ctx       Context

	Written []Written // every WriteFile, in order
	Posted  []Posted  // every Post, in order
	Asked   []string  // every prompt, in order
	Stdout  strings.Builder
	Stderr  strings.Builder
}

type Written struct{ Path, Content string }

type Posted struct{ URL, Body string }

// NewFake returns a Fake with a pinned context, so `context.env.today` is
// deterministic and a test that formats a date does not fail at midnight.
func NewFake() *Fake {
	return &Fake{
		Responses: map[string]string{},
		Files:     map[string]string{},
		Shells:    map[string]Shell{},
		Tools:     map[string]func(object.Value) (object.Value, error){},
		Ctx: Context{
			User:        "tester",
			LastMessage: "run the report",
			Messages:    []string{"hello", "run the report"},
			Cwd:         "/work",
			OS:          "testos",
			Today:       "2026-08-07",
		},
	}
}

func (h *Fake) Out() io.Writer { return &h.Stdout }

func (h *Fake) Err() io.Writer { return &h.Stderr }

func (h *Fake) Context() Context { return h.Ctx }

func (h *Fake) Fetch(url string) (string, error) {
	body, ok := h.Responses[url]
	if !ok {
		return "", fmt.Errorf("connection refused")
	}
	return body, nil
}

func (h *Fake) Post(url, body string) (string, error) {
	if _, ok := h.Responses[url]; !ok {
		return "", fmt.Errorf("connection refused")
	}
	h.Posted = append(h.Posted, Posted{URL: url, Body: body})
	return h.Responses[url], nil
}

func (h *Fake) ReadFile(path string) (string, error) {
	body, ok := h.Files[path]
	if !ok {
		return "", fmt.Errorf("no such file: %s", path)
	}
	return body, nil
}

func (h *Fake) WriteFile(path, content string) error {
	h.Written = append(h.Written, Written{Path: path, Content: content})
	h.Files[path] = content
	return nil
}

func (h *Fake) Run(command string) (Shell, error) {
	out, ok := h.Shells[command]
	if !ok {
		return Shell{Code: 127, Stderr: "command not found"}, nil
	}
	return out, nil
}

func (h *Fake) Ask(prompt string) (string, error) {
	h.Asked = append(h.Asked, prompt)
	if len(h.Answers) == 0 {
		return "", fmt.Errorf("no answer available on stdin")
	}
	answer := h.Answers[0]
	h.Answers = h.Answers[1:]
	return answer, nil
}

// ToolNames returns the bound tools in sorted order. Map iteration order would
// make `ask tools` produce a different list every run, and a test that passes
// four times in five is worse than no test.
func (h *Fake) ToolNames() []string {
	names := make([]string, 0, len(h.Tools))
	for name := range h.Tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (h *Fake) CallTool(name string, args object.Value) (object.Value, error) {
	fn, ok := h.Tools[name]
	if !ok {
		return nil, fmt.Errorf("no tool named %q is bound in this environment", name)
	}
	return fn(args)
}
