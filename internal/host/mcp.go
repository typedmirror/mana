package host

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/typedmirror/mana/internal/object"
)

// MCP bridges one MCP server into the module system (D-053). The server is a
// subprocess speaking newline-delimited JSON-RPC on stdio; it starts lazily on
// first use, and a call that hits the deadline kills it — a hung server is
// dead, not waited on. The next call starts it fresh.
//
// The MCP tool name is the call's target (D-028), `with { … }` is the
// arguments object verbatim, and the caller's `--` line travels in `_meta`
// under "mana/intent" — the intent channel does not terminate at a protocol
// boundary either.
type MCP struct {
	name    string
	command string
	timeout time.Duration
	effects []string // declared via MANA_MCP_<NAME>_EFFECTS; nil = undeclared (D-056)

	mu     sync.Mutex // serializes calls; guards stdin, scan, errBuf, nextID, tools
	stdin  io.WriteCloser
	scan   *bufio.Scanner
	errBuf *capped
	nextID int
	tools  []string

	// procMu guards proc and alive separately, because kill() can fire from
	// the deadline timer's goroutine while a call holds mu.
	procMu   sync.Mutex
	proc     *exec.Cmd
	alive    bool
	timedOut atomic.Bool
}

// NewMCP builds a bridge for one server. The command line runs through
// `$SHELL -c`, the same inheritance `run` has (D-006).
func NewMCP(name, command string) *MCP {
	return &MCP{name: name, command: command, timeout: DefaultTimeout}
}

// MCPFromEnv scans the environment for MANA_MCP_<NAME> entries and returns a
// bridge per server, named <name> lowercased, sorted for stable registration.
func MCPFromEnv() []*MCP {
	var out []*MCP
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(k, "MANA_MCP_") || v == "" {
			continue
		}
		// MANA_MCP_<NAME>_EFFECTS declares a footprint, not a server (D-056).
		if strings.HasSuffix(k, "_EFFECTS") {
			continue
		}
		name := strings.ToLower(strings.TrimPrefix(k, "MANA_MCP_"))
		if name == "" {
			continue
		}
		m := NewMCP(name, v)
		if decl := os.Getenv(k + "_EFFECTS"); decl != "" {
			for _, e := range strings.Split(decl, ",") {
				if e = strings.TrimSpace(e); e != "" {
					m.effects = append(m.effects, e)
				}
			}
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// Effects reports the declared footprint, or nil for undeclared — the bridge
// cannot know what a server does downstream, and guessing would be a
// plausible-looking lie (D-056).
func (m *MCP) Effects() []string { return m.effects }

// SetTimeout bounds each server call. Wired from --timeout (D-059): a hung
// server is killed at the deadline the operator actually asked for, not at
// the host default two minutes later.
func (m *MCP) SetTimeout(d time.Duration) {
	if d > 0 {
		m.timeout = d
	}
}

func (m *MCP) Name() string { return m.name }

// Clauses is nil: the built-in keywords suffice, with `with` carrying the
// arguments. Tool names live in the target, not the clause set — spec §7.4's
// sketch said otherwise and was corrected (D-053).
func (m *MCP) Clauses() []string { return nil }

func (m *MCP) Execute(call Call) object.Value {
	tool, bad := m.toolName(call)
	if bad != nil {
		return bad
	}
	args, bad := m.arguments(call)
	if bad != nil {
		return bad
	}
	format, bad := m.format(call)
	if bad != nil {
		return bad
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.isAlive() {
		if err := m.start(); err != nil {
			bad := Fail("cannot start mcp server %q: %v", m.name, err)
			bad.Suggestion = fmt.Sprintf("check the command in MANA_MCP_%s", strings.ToUpper(m.name))
			return bad
		}
	}
	if len(m.tools) > 0 && !contains(m.tools, tool) {
		bad := Fail("mcp server %q has no tool %q", m.name, tool)
		bad.Suggestion = "available tools: [" + strings.Join(m.tools, ", ") + "]"
		return bad
	}

	params := map[string]any{"name": tool, "arguments": args}
	if call.Intent != "" {
		params["_meta"] = map[string]string{"mana/intent": call.Intent}
	}
	raw, rpcErr := m.rpc("tools/call", params)
	if rpcErr != nil {
		bad := Fail("mcp server %q: %v", m.name, rpcErr)
		if tail := strings.TrimSpace(m.errBuf.String()); tail != "" {
			bad.Reason += " — server stderr: " + tailOf(tail, 240)
		}
		return bad
	}
	return m.decodeResult(tool, raw, format)
}

// toolName resolves which tool is being called: the bare-word target, or the
// first string argument when the tool's name is not writable as a word.
func (m *MCP) toolName(call Call) (string, *object.Err) {
	if call.Target != "" {
		return call.Target, nil
	}
	if len(call.Args) > 0 {
		if s, ok := call.Args[0].(object.String); ok {
			return string(s), nil
		}
	}
	bad := Fail("%s needs a tool name", m.name)
	bad.Suggestion = fmt.Sprintf(`write %s <tool> with { … }, or %s "tool-name" with { … }`, m.name, m.name)
	return "", bad
}

// arguments turns the `with` record into the MCP arguments object, verbatim.
func (m *MCP) arguments(call Call) (map[string]json.RawMessage, *object.Err) {
	args := map[string]json.RawMessage{}
	v, ok := call.Clauses["with"]
	if !ok {
		return args, nil
	}
	rec, isRec := v.(*object.Record)
	if !isRec {
		bad := Fail("%s arguments must be a record, got %s", m.name, v.Type())
		bad.Suggestion = "write with { key: value, … }"
		return nil, bad
	}
	for _, k := range rec.Keys() {
		val, _ := rec.Get(k)
		args[k] = json.RawMessage(object.JSON(val))
	}
	return args, nil
}

func (m *MCP) format(call Call) (string, *object.Err) {
	v, ok := call.Clauses["as"]
	if !ok {
		return "text", nil
	}
	f := strings.ToLower(word(v))
	if f != "text" && f != "json" {
		bad := Fail("%s cannot answer as %q", m.name, word(v))
		bad.Suggestion = "valid formats: text, json"
		return "", bad
	}
	return f, nil
}

// start launches the server and completes the MCP handshake: initialize,
// the initialized notification, then tools/list for the vocabulary.
func (m *MCP) start() error {
	sh := os.Getenv("SHELL")
	if sh == "" {
		sh = "/bin/sh"
	}
	cmd := exec.Command(sh, "-c", m.command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	m.errBuf = &capped{}
	cmd.Stderr = m.errBuf
	if err := cmd.Start(); err != nil {
		return err
	}
	m.procMu.Lock()
	m.proc = cmd
	m.alive = true
	m.procMu.Unlock()
	m.stdin = stdin
	m.scan = bufio.NewScanner(stdout)
	m.scan.Buffer(make([]byte, 0, 64<<10), 4<<20)
	m.nextID = 0

	if _, err := m.rpc("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "mana", "version": "0.2"},
	}); err != nil {
		m.kill()
		return fmt.Errorf("initialize: %w", err)
	}
	m.notify("notifications/initialized")

	raw, err := m.rpc("tools/list", map[string]any{})
	if err != nil {
		m.kill()
		return fmt.Errorf("tools/list: %w", err)
	}
	var listed struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &listed); err == nil {
		m.tools = m.tools[:0]
		for _, t := range listed.Tools {
			m.tools = append(m.tools, t.Name)
		}
		sort.Strings(m.tools)
	}
	return nil
}

// rpc performs one request/response roundtrip under the deadline. On
// deadline, the server's whole process group is killed; the scanner then
// sees EOF and the call reports the timeout.
func (m *MCP) rpc(method string, params any) (json.RawMessage, error) {
	m.nextID++
	id := m.nextID
	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if _, err := m.stdin.Write(append(req, '\n')); err != nil {
		m.kill()
		return nil, fmt.Errorf("server is gone: %v", err)
	}

	m.timedOut.Store(false)
	timer := time.AfterFunc(m.timeout, func() { m.timedOut.Store(true); m.kill() })
	defer timer.Stop()

	for m.scan.Scan() {
		var resp struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(m.scan.Bytes(), &resp); err != nil {
			continue // not a response; a server may log or notify
		}
		if resp.ID == nil || *resp.ID != id {
			continue // a notification, or an answer to someone this call is not
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("server error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, nil
	}
	m.kill()
	if m.timedOut.Load() {
		return nil, fmt.Errorf("no answer within %s — server killed; the next call restarts it", m.timeout)
	}
	return nil, fmt.Errorf("server closed the stream")
}

func (m *MCP) notify(method string) {
	msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	m.stdin.Write(append(msg, '\n'))
}

func (m *MCP) isAlive() bool {
	m.procMu.Lock()
	defer m.procMu.Unlock()
	return m.alive
}

func (m *MCP) kill() {
	m.procMu.Lock()
	defer m.procMu.Unlock()
	if m.proc != nil && m.proc.Process != nil {
		syscall.Kill(-m.proc.Process.Pid, syscall.SIGKILL)
		p := m.proc
		go p.Wait() // reap; the result no longer matters
	}
	// Cleared so a second kill — the timer and the EOF path can both arrive —
	// cannot Wait() on the same process twice.
	m.proc = nil
	m.alive = false
}

// decodeResult maps an MCP tool result to a value: text content joined, a
// declared tool error to an error value, `as json` parsed hard (D-044).
func (m *MCP) decodeResult(tool string, raw json.RawMessage, format string) object.Value {
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return Fail("mcp tool %q answered in a shape this bridge does not know: %v", tool, err)
	}
	var parts []string
	for _, c := range res.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		} else {
			parts = append(parts, "["+c.Type+" content]")
		}
	}
	text := strings.Join(parts, "\n")
	if res.IsError {
		return Fail("mcp tool %q failed: %s", tool, tailOf(strings.TrimSpace(text), 240))
	}
	if strings.TrimSpace(text) == "" {
		return Fail("mcp tool %q returned nothing", tool)
	}
	if format == "json" {
		v, err := object.ParseJSON(strings.TrimSpace(text))
		if err != nil {
			return Fail("mcp tool %q reply is not the json that was asked for: %v", tool, err)
		}
		return v
	}
	return object.String(strings.TrimSuffix(text, "\n"))
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// tailOf keeps the last n characters of a diagnostic — the reader is a
// context window, and the end is where the reason lives.
func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
