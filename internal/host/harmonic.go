package host

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Harmonic routes mana's effects through a guarded harmonic kernel (D-064):
// run as an in-kernel subprocess, read/write as in-kernel file operations,
// fetch/post as in-kernel network — every axis under the kernel's guard,
// which is process-scoped and therefore only sees what executes inside it
// (G-seam finding #1).
//
// It wraps a base host and overrides the effect verbs; Out/Err/Ask/Context
// and the module registry stay with the base. The exec contract is
// harmonic's, authored not guessed: `harmonic exec <kernel> -f <cell>`
// returns one JSON document; exit 0 is success, exit 3 is a capability
// denial whose evalue carries the inline provenance ref.
type Harmonic struct {
	*Real
	cli      string // the harmonic binary; MANA_HARMONIC_CMD overrides for stubs
	kernelID string
	stateDir string
	timeout  time.Duration

	// execMu serializes cell execs against the one kernel this host drives —
	// the reference shim holds a per-kernel lock for the same reason: mana's
	// wave-1 fires acts concurrently, and the host must tolerate that by
	// serializing, not racing (parity finding, 2026-09-05).
	execMu sync.Mutex
}

// NewHarmonic wraps a Real host so effects execute inside the given kernel.
// The (kernelID, stateDir) pair is the session handle per the contract.
func NewHarmonic(base *Real, kernelID, stateDir string) *Harmonic {
	cli := os.Getenv("MANA_HARMONIC_CMD")
	if cli == "" {
		cli = "harmonic"
	}
	return &Harmonic{Real: base, cli: cli, kernelID: kernelID, stateDir: stateDir, timeout: DefaultTimeout}
}

// SetTimeout bounds each kernel exec (D-059).
func (h *Harmonic) SetTimeout(d time.Duration) {
	if d > 0 {
		h.timeout = d
	}
}

// execResponse is the contract's response document. cell_id is deliberately
// NOT parsed: the host never needed it, and typing a field you do not use is
// how the parity gate caught this struct lying (cell_id is an integer in the
// real contract; the old string field made every real response unparseable
// while the fake — which had drifted to match the assumption — passed).
type execResponse struct {
	Status  string `json:"status"`
	Outputs []struct {
		Type   string `json:"type"`
		Name   string `json:"name"`
		Text   string `json:"text"`
		Ename  string `json:"ename"`
		Evalue string `json:"evalue"`
	} `json:"outputs"`
}

// cell runs one generated Python cell under the host's default deadline.
func (h *Harmonic) cell(source string) (*execResponse, int, error) {
	return h.cellWithin(source, h.timeout)
}

// cellWithin runs one generated Python cell in the kernel and returns the
// parsed response plus the CLI's exit code — 3 is the denial channel. Execs
// serialize on the per-kernel mutex.
func (h *Harmonic) cellWithin(source string, deadline time.Duration) (*execResponse, int, error) {
	h.execMu.Lock()
	defer h.execMu.Unlock()
	f, err := os.CreateTemp("", "mana-cell-*.py")
	if err != nil {
		return nil, -1, err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(source); err != nil {
		f.Close()
		return nil, -1, err
	}
	f.Close()

	cmd := exec.Command(h.cli, "exec", h.kernelID, "-f", f.Name())
	cmd.Env = append(os.Environ(), "HARMONIC_STATE_DIR="+h.stateDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr capped
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	timer := time.AfterFunc(deadline, func() {
		if cmd.Process != nil {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	})
	defer timer.Stop()

	runErr := cmd.Run()
	code := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if asExitError(runErr, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			return nil, -1, fmt.Errorf("cannot run %q: %v", h.cli, runErr)
		}
	}
	var resp execResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &resp); err != nil {
		return nil, code, fmt.Errorf("kernel answered in a shape this host does not know (exit %d): %v — stderr: %s", code, err, tailOf(strings.TrimSpace(stderr.String()), 240))
	}
	return &resp, code, nil
}

// pick gathers a response's streams and error text.
func (r *execResponse) pick() (stdout, stderr, evalue string) {
	for _, o := range r.Outputs {
		switch {
		case o.Type == "stream" && o.Name == "Stdout":
			stdout += o.Text
		case o.Type == "stream" && o.Name == "Stderr":
			stderr += o.Text
		case o.Type == "error":
			evalue = o.Evalue
		case o.Type == "result" && stdout == "":
			stdout = o.Text
		}
	}
	return
}

// b64 embeds arbitrary bytes safely in generated source.
func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// Run executes the line as an in-kernel subprocess, argv-direct so the guard
// sees the real basename (never `sh`). The with-env arrives as DATA (D-065)
// and merges over os.environ; user-written leading KEY=value words in the
// line itself are still lifted, exactly as a POSIX shell treats leading
// assignments. The guard fires on the Popen regardless of env.
func (h *Harmonic) Run(command string, env map[string]string, timeout time.Duration) (Shell, error) {
	// A local deadline, never a field write: concurrent acts share this host,
	// and the old mutation raced every other call's kill-timer read — a torn
	// duration fires the timer at once and truncates the exec mid-output.
	deadline := h.timeout
	if timeout > 0 {
		deadline = timeout
	}
	if env == nil {
		env = map[string]string{}
	}
	envJSON, _ := json.Marshal(env)
	src := fmt.Sprintf(`# MANA_OP {"op":"run","b64":%q}
import base64, json, os, re, shlex, subprocess, sys
line = base64.b64decode(%q).decode()
argv = shlex.split(line)
env = dict(os.environ)
env.update(json.loads(base64.b64decode(%q).decode()))
while argv and re.match(r"^[A-Za-z_][A-Za-z0-9_]*=", argv[0]):
    k, _, v = argv.pop(0).partition("=")
    env[k] = v
if not argv:
    print(json.dumps({"rc": 2, "out": "", "err": "empty command"})); sys.exit(0)
c = subprocess.run(argv, env=env, capture_output=True, text=True)
print(json.dumps({"rc": c.returncode, "out": c.stdout, "err": c.stderr}))
`, b64(command), b64(command), b64(string(envJSON)))

	resp, code, err := h.cellWithin(src, deadline)
	if err != nil {
		return Shell{}, err
	}
	stdout, stderrText, evalue := resp.pick()
	if code == 3 || resp.Status == "denied" {
		// The denial channel: evalue carries the inline provenance ref, and
		// exit 3 mirrors the reference shim so the parity gate can compare
		// reports byte for byte.
		return Shell{Code: 3, Stderr: evalue}, nil
	}
	if code != 0 || resp.Status == "error" {
		if evalue == "" {
			evalue = stderrText
		}
		return Shell{Code: 1, Stderr: evalue}, nil
	}
	// The cell prints one JSON line carrying the inner command's outcome.
	var inner struct {
		RC  int    `json:"rc"`
		Out string `json:"out"`
		Err string `json:"err"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &inner); err != nil {
		return Shell{}, fmt.Errorf("kernel cell answered in a shape this host does not know: %v", err)
	}
	return Shell{Code: inner.RC, Stdout: inner.Out, Stderr: inner.Err}, nil
}

// fileCell is the shared shape of in-kernel read/write.
func (h *Harmonic) fileCell(src string) (string, error) {
	resp, code, err := h.cell(src)
	if err != nil {
		return "", err
	}
	stdout, stderrText, evalue := resp.pick()
	if code == 3 || resp.Status == "denied" {
		return "", fmt.Errorf("%s", evalue)
	}
	if code != 0 || resp.Status == "error" {
		if evalue == "" {
			evalue = stderrText
		}
		return "", fmt.Errorf("%s", evalue)
	}
	return stdout, nil
}

func (h *Harmonic) ReadFile(path string) (string, error) {
	src := fmt.Sprintf(`# MANA_OP {"op":"read","b64":%q}
import base64, sys
p = base64.b64decode(%q).decode()
sys.stdout.write(open(p).read())
`, b64(path), b64(path))
	return h.fileCell(src)
}

func (h *Harmonic) WriteFile(path, content string) error {
	src := fmt.Sprintf(`# MANA_OP {"op":"write","b64":%q}
import base64
p = base64.b64decode(%q).decode()
open(p, "w").write(base64.b64decode(%q).decode())
print("ok")
`, b64(path), b64(path), b64(content))
	_, err := h.fileCell(src)
	return err
}

func (h *Harmonic) Fetch(url string) (string, error) {
	src := fmt.Sprintf(`# MANA_OP {"op":"fetch","b64":%q}
import base64, sys, urllib.request
u = base64.b64decode(%q).decode()
sys.stdout.write(urllib.request.urlopen(u, timeout=30).read().decode())
`, b64(url), b64(url))
	return h.fileCell(src)
}

func (h *Harmonic) Post(url, body string) (string, error) {
	src := fmt.Sprintf(`# MANA_OP {"op":"post","b64":%q}
import base64, sys, urllib.request
u = base64.b64decode(%q).decode()
d = base64.b64decode(%q)
req = urllib.request.Request(u, data=d, headers={"Content-Type": "application/json"})
sys.stdout.write(urllib.request.urlopen(req, timeout=30).read().decode())
`, b64(url), b64(url), b64(body))
	return h.fileCell(src)
}

// Effects declares nothing new: the host is a router, and the guard is the
// authority on what its kernel permits.
var _ Host = (*Harmonic)(nil)
