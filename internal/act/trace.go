package act

import (
	"fmt"
	"strings"
	"time"
)

// Trace renders a job's execution record (v2 §14.5).
//
// Nothing here is instrumentation. Every field it prints — timing, status, tool
// scope, dependency state, the reasoning in force — is something an act
// produces by existing. That is the claim §14.5 makes, and this function is
// what tests it: if a trace needed data the scheduler does not already keep,
// the claim would be false.
func Trace(r *Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Trace: %d act(s) in %s\n", len(r.Outcomes), short(r.Elapsed))

	for i, o := range r.Outcomes {
		branch := "├─"
		if i == len(r.Outcomes)-1 {
			branch = "└─"
		}
		cont := "│ "
		if i == len(r.Outcomes)-1 {
			cont = "  "
		}

		name := o.Name
		if name == "" {
			name = "(flat script)"
		}
		fmt.Fprintf(&b, "  %s %-22s %-18s %s\n", branch, name, window(o), mark(o.Status))

		if o.Reason != "" {
			fmt.Fprintf(&b, "  %s     reason: %s\n", cont, o.Reason)
		}
		if o.Err != nil {
			fmt.Fprintf(&b, "  %s     reason: %s\n", cont, o.Err.Reason)
		}
		// Each `--` block, in order: the model's own segmentation of its work.
		for _, st := range o.Steps {
			label := st.Intent
			if label == "" {
				label = "(no intent given)"
			}
			state := "·"
			if st.Status == "failed" {
				state = "✗"
			}
			fmt.Fprintf(&b, "  %s     %s %-8s %s\n", cont, state, short(st.Duration), label)
			for _, note := range st.Notes {
				fmt.Fprintf(&b, "  %s        note: %s\n", cont, note)
			}
		}
		if len(o.Steps) == 0 && len(o.Intents) > 0 {
			fmt.Fprintf(&b, "  %s     intent: %q\n", cont, o.Intents[len(o.Intents)-1])
		}
		if len(o.Uses) > 0 {
			fmt.Fprintf(&b, "  %s     tools:  [%s]\n", cont, strings.Join(o.Uses, ", "))
		}
		if len(o.Depends) > 0 {
			fmt.Fprintf(&b, "  %s     deps:   [%s]\n", cont, strings.Join(o.Depends, ", "))
		}
		if o.Attempts > 1 {
			fmt.Fprintf(&b, "  %s     tried:  %d times\n", cont, o.Attempts)
		}
	}
	return b.String()
}

func mark(s Status) string {
	switch s {
	case Succeeded:
		return "ok"
	case Failed:
		return "FAILED"
	}
	return "skipped"
}

// window renders when an act ran, relative to the start of the job.
func window(o Outcome) string {
	if o.Status == Skipped {
		return "—"
	}
	return "[" + short(o.Started) + " → " + short(o.Started+o.Duration) + "]"
}

// short formats a duration the way a trace wants to read: whole milliseconds
// until a second, then seconds.
func short(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return "0ms"
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}
