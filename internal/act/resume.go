package act

// Cross-invocation resume (D-066, resolving D-014): the report is the state
// file. A prior --json report carries, for every act that succeeded, the
// identity of the text that earned the result and the result itself. Feeding
// that report back with --resume rebuilds a Prior — the same structure serve
// uses for in-session reuse (D-054) — so an interrupted job finishes without
// re-firing the effects of the acts that already succeeded. The flag is the
// freshness assertion: no flag, no reuse, ever.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/typedmirror/mana/internal/ast"
	"github.com/typedmirror/mana/internal/object"
)

// identityOf names the exact text an act result was earned by. The dependency
// graph plus this hash is the whole staleness model: identical text with an
// unbroken chain of unchanged ancestors is what "still true" means (D-054,
// extended across invocations by D-066).
func identityOf(a *ast.Act) string {
	sum := sha256.Sum256([]byte(a.String()))
	return hex.EncodeToString(sum[:8])
}

// resumeEntry is one act's contribution to the integrity hash: the fields a
// resume actually trusts, nothing else.
type resumeEntry struct {
	name     string
	identity string
	result   []byte // compact JSON, empty when the act sent no result
}

// integrityOf seals the resumable subset of a report. Both the writer and the
// loader compute it from the same fields, so a hand-edited result or identity
// no longer matches and the resume refuses loudly (I1) instead of quietly
// running on tampered state.
func integrityOf(entries []resumeEntry) string {
	h := sha256.New()
	for _, e := range entries {
		h.Write([]byte(e.name))
		h.Write([]byte{0})
		h.Write([]byte(e.identity))
		h.Write([]byte{0})
		h.Write(e.result)
		h.Write([]byte{0x1e})
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// resumeEntries extracts the sealed subset from finished outcomes, in report
// order. Only named acts that succeeded (or were themselves reused) count —
// a flat script is one unnamed act and always re-runs.
func resumeEntries(outcomes []Outcome) []resumeEntry {
	var entries []resumeEntry
	for _, o := range outcomes {
		if o.Status != Succeeded && o.Status != Reused {
			continue
		}
		if o.Name == "" || o.Identity == "" {
			continue
		}
		e := resumeEntry{name: o.Name, identity: o.Identity}
		if o.HasResult {
			e.result = compact([]byte(object.JSON(o.Result)))
		}
		entries = append(entries, e)
	}
	return entries
}

func compact(raw []byte) []byte {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return raw
	}
	return buf.Bytes()
}

// reportShape is the subset of the report wire format a resume reads. It is
// deliberately declared a second time: the loader trusts the file, not the
// writer's internal types.
type reportShape struct {
	Integrity string `json:"integrity"`
	Acts      []struct {
		Name      string          `json:"name"`
		Status    string          `json:"status"`
		Identity  string          `json:"identity"`
		HasResult bool            `json:"has_result"`
		Result    json.RawMessage `json:"result"`
	} `json:"acts"`
}

// PriorFromReport rebuilds a Prior from a prior run's --json report. The tag
// it returns names the report (its integrity hash) for the resumed_from
// lineage field. A report that fails integrity, carries no seal, or holds an
// unparseable result is refused with the reason — resuming on doubtful state
// silently would be the defect this language exists to remove.
func PriorFromReport(data []byte) (*Prior, string, error) {
	var r reportShape
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, "", fmt.Errorf("not a readable report: %v", err)
	}

	var entries []resumeEntry
	p := &Prior{acts: map[string]priorAct{}}
	for _, a := range r.Acts {
		if Status(a.Status) != Succeeded && Status(a.Status) != Reused {
			continue
		}
		if a.Name == "" || a.Identity == "" {
			continue
		}
		e := resumeEntry{name: a.Name, identity: a.Identity}
		pa := priorAct{identity: a.Identity}
		if a.HasResult {
			v, err := object.ParseJSON(string(a.Result))
			if err != nil {
				return nil, "", fmt.Errorf("act %q: result does not parse: %v", a.Name, err)
			}
			pa.result, pa.hasResult = v, true
			e.result = compact(a.Result)
		}
		entries = append(entries, e)
		p.acts[a.Name] = pa
	}

	if len(entries) == 0 {
		// Nothing succeeded, so nothing is reused and nothing needs trusting:
		// the resume runs everything, which is exactly what the caller asked
		// to finish.
		return p, "none", nil
	}
	if r.Integrity == "" {
		return nil, "", fmt.Errorf("report carries no integrity seal — it predates resume or was stripped; re-run without --resume")
	}
	if want := integrityOf(entries); r.Integrity != want {
		return nil, "", fmt.Errorf("report integrity mismatch (sealed %s, found %s) — the report was edited after it was written; re-run without --resume", r.Integrity, want)
	}
	return p, r.Integrity, nil
}
