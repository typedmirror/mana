package act

import (
	"encoding/json"

	"github.com/typedmirror/mana/internal/object"
)

// JSON renders a report for its actual reader: a model deciding its next move
// (D-048). The order is the reading order — did it work, what came out, what
// failed and why, what never ran — and a failure appears in exactly one
// place. The wire shape is declared here rather than reusing the internal
// types, so changing a field inside the scheduler cannot silently change what
// callers receive.
func JSON(r *Report, output string) ([]byte, error) {
	out := jobJSON{
		OK:        r.OK(),
		Output:    output,
		ElapsedMs: r.Elapsed.Milliseconds(),
	}
	if r.Err != nil {
		out.Error = errJSON("", r.Err)
	}
	for _, o := range r.Outcomes {
		if o.Err != nil {
			out.Failures = append(out.Failures, *errJSON(o.Name, o.Err))
		}
		if o.Status == Skipped {
			out.Skipped = append(out.Skipped, o.Name)
		}
		out.Acts = append(out.Acts, actJSON(o))
	}
	return json.MarshalIndent(out, "", "  ")
}

type jobJSON struct {
	OK bool `json:"ok"`
	// Output is everything the script sent to `output`, gathered here so one
	// document holds the whole answer.
	Output string `json:"output,omitempty"`
	// Failures is the triage list: every failure once, act-attributed, in
	// completion order. The acts and steps below carry status only.
	Failures []failJSON `json:"failures,omitempty"`
	// Skipped names the acts that never ran. Absence from here plus a `halted`
	// step is how a reader knows what the job did not do.
	Skipped   []string    `json:"skipped,omitempty"`
	Error     *failJSON   `json:"error,omitempty"` // a job that could not start at all
	Acts      []actRecord `json:"acts"`
	ElapsedMs int64       `json:"elapsed_ms"`
}

type actRecord struct {
	Name string `json:"name"`
	// Status is "ok", "failed", or "skipped". Skipped is deliberately not a
	// success: the act never ran. The failure itself lives in the job's
	// `failures` list, once.
	Status     string       `json:"status"`
	Result     any          `json:"result,omitempty"`
	Reason     string       `json:"reason,omitempty"` // why it was skipped
	Steps      []stepRecord `json:"steps,omitempty"`
	Uses       []string     `json:"uses,omitempty"`
	Depends    []string     `json:"depends,omitempty"`
	Attempts   int          `json:"attempts,omitempty"` // present only when retried
	StartedMs  int64        `json:"started_ms,omitempty"`
	DurationMs int64        `json:"duration_ms,omitempty"`
}

// stepRecord is one `--` block — the model's own segmentation of its work,
// handed back in the shape it wrote. Status "halted" means a failure created
// earlier reached a bare statement here and stopped the run.
type stepRecord struct {
	Intent string `json:"intent"`
	Status string `json:"status"`
	// Line appears only when something went wrong here; a healthy step's line
	// number is noise.
	Line int `json:"line,omitempty"`
	// Effects are the calls that changed something outside the process. The
	// first question after a partial failure is "what did it already do".
	Effects    []string `json:"effects,omitempty"`
	Notes      []string `json:"notes,omitempty"`
	DurationMs int64    `json:"duration_ms,omitempty"`
}

type failJSON struct {
	Act        string `json:"act,omitempty"` // "" is the unnamed act of a flat script
	At         string `json:"at"`
	Intent     string `json:"intent"`
	Reason     string `json:"reason"`
	Suggestion string `json:"suggestion,omitempty"`
	Line       int    `json:"line,omitempty"`
}

func actJSON(o Outcome) actRecord {
	rec := actRecord{
		Name:       o.Name,
		Status:     string(o.Status),
		Reason:     o.Reason,
		Uses:       o.Uses,
		Depends:    o.Depends,
		StartedMs:  o.Started.Milliseconds(),
		DurationMs: o.Duration.Milliseconds(),
	}
	if o.Attempts > 1 {
		rec.Attempts = o.Attempts
	}
	if o.HasResult {
		rec.Result = wire(o.Result)
	}
	for _, s := range o.Steps {
		step := stepRecord{
			Intent:     s.Intent,
			Status:     s.Status,
			Effects:    s.Effects,
			Notes:      s.Notes,
			DurationMs: s.Duration.Milliseconds(),
		}
		if s.Status != "ok" {
			step.Line = s.Line
		}
		rec.Steps = append(rec.Steps, step)
	}
	return rec
}

func errJSON(act string, e *object.Err) *failJSON {
	return &failJSON{
		Act:        act,
		At:         e.At,
		Intent:     e.Intent,
		Reason:     e.Reason,
		Suggestion: e.Suggestion,
		Line:       e.Line,
	}
}

// wire converts a Mana value to something encoding/json can emit, preserving
// record key order by using json.RawMessage from the value's own serializer.
func wire(v object.Value) any {
	return json.RawMessage(object.JSON(v))
}
