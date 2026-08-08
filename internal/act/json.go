package act

import (
	"encoding/json"

	"github.com/typedmirror/mana/internal/object"
)

// JSON renders a report for a machine.
//
// The usual consumer of a Mana run is a model, not a terminal. It fired one
// artifact and is reading one result, so that result is its context: a tree
// drawn with box characters costs tokens to carry and work to parse, and both
// are avoidable. The wire shape is declared here rather than reusing the
// internal types, so changing a field inside the scheduler cannot silently
// change what callers receive.
func JSON(r *Report, output string) ([]byte, error) {
	out := jobJSON{
		OK:        r.OK(),
		ElapsedMs: r.Elapsed.Milliseconds(),
		Output:    output,
	}
	if r.Err != nil {
		out.Error = errJSON(r.Err)
	}
	for _, o := range r.Outcomes {
		out.Acts = append(out.Acts, actJSON(o))
	}
	return json.MarshalIndent(out, "", "  ")
}

type jobJSON struct {
	OK        bool      `json:"ok"`
	ElapsedMs int64     `json:"elapsed_ms"`
	Error     *failJSON `json:"error,omitempty"` // a job that could not start at all
	// Output is everything the script sent to `output`, gathered here so one
	// document holds the whole answer.
	Output string      `json:"output,omitempty"`
	Acts   []actRecord `json:"acts"`
}

type actRecord struct {
	Name string `json:"name"`
	// Status is "ok", "failed", or "skipped". Skipped is deliberately not a
	// success: the act never ran.
	Status     string       `json:"status"`
	Result     any          `json:"result,omitempty"`
	Error      *failJSON    `json:"error,omitempty"`
	Reason     string       `json:"reason,omitempty"` // why it was skipped
	Steps      []stepRecord `json:"steps,omitempty"`
	Uses       []string     `json:"uses,omitempty"`
	Depends    []string     `json:"depends,omitempty"`
	Attempts   int          `json:"attempts,omitempty"`
	StartedMs  int64        `json:"started_ms"`
	DurationMs int64        `json:"duration_ms"`
}

// stepRecord is one `--` block — the model's own segmentation of its work,
// handed back in the shape it wrote.
type stepRecord struct {
	Intent     string    `json:"intent"`
	Status     string    `json:"status"`
	Line       int       `json:"line,omitempty"`
	Error      *failJSON `json:"error,omitempty"`
	Notes      []string  `json:"notes,omitempty"`
	DurationMs int64     `json:"duration_ms"`
}

type failJSON struct {
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
		Attempts:   o.Attempts,
		StartedMs:  o.Started.Milliseconds(),
		DurationMs: o.Duration.Milliseconds(),
	}
	if o.HasResult {
		rec.Result = wire(o.Result)
	}
	if o.Err != nil {
		rec.Error = errJSON(o.Err)
	}
	for _, s := range o.Steps {
		step := stepRecord{
			Intent:     s.Intent,
			Status:     s.Status,
			Line:       s.Line,
			Notes:      s.Notes,
			DurationMs: s.Duration.Milliseconds(),
		}
		if s.Err != nil {
			step.Error = errJSON(s.Err)
		}
		rec.Steps = append(rec.Steps, step)
	}
	return rec
}

func errJSON(e *object.Err) *failJSON {
	return &failJSON{
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
