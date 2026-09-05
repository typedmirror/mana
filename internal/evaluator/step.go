package evaluator

import (
	"fmt"
	"time"

	"github.com/typedmirror/mana/internal/host"
	"github.com/typedmirror/mana/internal/object"
)

// hostDefaultTimeout mirrors the host's default, for reporting.
const hostDefaultTimeout = host.DefaultTimeout

// Step is one `--` block: the reasoning, and everything that happened under it.
//
// This is the unit the language was named for. A model writing a task already
// emits a `--` line before each thing it does, so the blocks are the model's own
// segmentation of its work — not a construct it has to learn. Reporting per
// block is what turns a script that returns one pass/fail into one that returns
// an outcome per step the model actually intended.
type Step struct {
	Intent string
	Line   int
	// Status is "ok", "failed" (this step created the failure), or "halted" —
	// a failure created earlier reached a bare statement here and stopped the
	// run. Halted is not ok: the step never accomplished its stated intent.
	Status string
	Err    *object.Err
	Notes  []string // observations worth keeping that are not the value
	// Effects are the calls that changed something outside the process — shell
	// commands, writes, sends that leave, module invocations. The first
	// question after a partial failure is "what did it already do", and reads
	// are excluded because they leave no wreckage (D-048).
	Effects []string
	// Reads witnesses what was observed — read paths and fetched URLs — in a
	// separate column because confidentiality and integrity are different
	// questions (D-057). An envelope that enforces fs_read deserves an
	// exercised-side witness on that axis.
	Reads    []string
	Duration time.Duration
}

// beginStep closes the running block and opens a new one. Called for every
// intent line, which is what makes `--` the boundary.
func (e *Evaluator) beginStep(intent string, line int) {
	e.closeStep()
	e.step = &Step{Intent: intent, Line: line, Status: "ok"}
	e.stepStart = e.now()
}

// closeStep files the running block, if there is one.
func (e *Evaluator) closeStep() {
	if e.step == nil {
		return
	}
	e.step.Duration = e.now().Sub(e.stepStart)
	e.steps = append(e.steps, *e.step)
	e.step = nil
}

// failStep marks the running block as the one that failed.
func (e *Evaluator) failStep(err *object.Err) {
	if e.step == nil {
		// A failure before any reasoning was written still belongs to a block,
		// so an unlabelled one is opened rather than dropping the record.
		e.beginStep("", err.Line)
	}
	e.step.Status = "failed"
	e.step.Err = err
}

// note records something worth reporting that is not the value: stderr from a
// command that succeeded, output that was cut, a deadline that fired. Without
// this they would be discarded, which is the defect this language exists to
// remove, appearing inside the runtime itself.
func (e *Evaluator) note(format string, args ...any) {
	if e.step == nil {
		e.beginStep("", 0)
	}
	e.step.Notes = append(e.step.Notes, fmt.Sprintf(format, args...))
}

// effect records a call that is about to change something outside the process,
// under the step whose reasoning fired it. Recorded at the attempt, not the
// success: a mutation that failed halfway is still wreckage.
func (e *Evaluator) effect(format string, args ...any) {
	if e.step == nil {
		e.beginStep("", 0)
	}
	e.step.Effects = append(e.step.Effects, bounded(fmt.Sprintf(format, args...)))
}

// observe records a read — a path or URL the step looked at (D-057).
func (e *Evaluator) observe(format string, args ...any) {
	if e.step == nil {
		e.beginStep("", 0)
	}
	e.step.Reads = append(e.step.Reads, bounded(fmt.Sprintf(format, args...)))
}

func bounded(s string) string {
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

// Steps returns the blocks this evaluator ran, in order.
func (e *Evaluator) Steps() []Step {
	e.closeStep()
	return e.steps
}

func (e *Evaluator) now() time.Time { return time.Now() }

// runTimeout is the deadline in force for a command.
func (e *Evaluator) runTimeout() time.Duration {
	if e.timeout > 0 {
		return e.timeout
	}
	return hostDefaultTimeout
}
