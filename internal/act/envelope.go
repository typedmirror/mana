package act

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/typedmirror/mana/internal/ast"
	"github.com/typedmirror/mana/internal/host"
	"github.com/typedmirror/mana/internal/object"
	"github.com/typedmirror/mana/internal/token"
)

// Envelope is one act's effect coordinate in the shape harmonic consumes
// (D-055): what the act may touch, derived from its verbs and its grants.
// grant_window is deliberately absent — the producer does not write the
// consumer's domain. Grant scope IS the act; the schedule turns scope into
// position on the timeline.
type Envelope struct {
	// Subprocess lists binaries the act may spawn. "*" means the act runs a
	// raw shell line — ⊤ by ruling (D-052), not by omission.
	Subprocess []string `json:"subprocess,omitempty"`
	Network    bool     `json:"network,omitempty"`
	// FSRead and FSWrite carry path literals as written; "*" marks a path
	// that is computed rather than written.
	FSRead  []string `json:"fs_read,omitempty"`
	FSWrite []string `json:"fs_write,omitempty"`
}

// ActEnvelope is the per-act record of the emission.
type ActEnvelope struct {
	Name    string   `json:"name"`
	Depends []string `json:"depends,omitempty"`
	// EffectLevel aligns with the two-phase gate: "pure" (no host contact),
	// "observe" (reads the world, changes nothing), "io" (changes the world).
	EffectLevel string       `json:"effect_level"`
	Envelope    Envelope     `json:"envelope"`
	Modules     []ModuleNote `json:"modules,omitempty"`
	// Top names every contributor that forced ⊤ on some axis, so a reader
	// can see exactly where the projection is honest rather than precise.
	Top []string `json:"top,omitempty"`
}

// ModuleNote records how a granted module contributed (D-056).
type ModuleNote struct {
	Name     string   `json:"name"`
	Effects  []string `json:"effects,omitempty"`
	Declared bool     `json:"declared"`
}

// EffectDeclarer is the optional interface a module implements to keep its
// grant from projecting to ⊤ (D-056). An empty slice means "pure".
type EffectDeclarer interface {
	Effects() []string
}

type familyJSON struct {
	Acts []ActEnvelope `json:"acts"`
}

// Envelopes derives the per-act envelope family without executing anything.
// It validates the graph the same way DryRun does, so an emission for an
// unrunnable job fails the same way the job would.
func Envelopes(prog *ast.Program, h host.Host) ([]byte, *object.Err) {
	acts, loose := Split(prog)
	if len(acts) == 0 {
		flat := flatAct(loose)
		fam := familyJSON{Acts: []ActEnvelope{envelopeOne(flat, h)}}
		out, _ := json.MarshalIndent(fam, "", "  ")
		return out, nil
	}
	byName, err := index(acts)
	if err != nil {
		return nil, err
	}
	if err := checkDependencies(acts, byName); err != nil {
		return nil, err
	}
	if err := checkCycles(acts, byName); err != nil {
		return nil, err
	}
	if err := resolveImports(acts, h); err != nil {
		return nil, err
	}
	fam := familyJSON{}
	for _, a := range acts {
		fam.Acts = append(fam.Acts, envelopeOne(a, h))
	}
	out, _ := json.MarshalIndent(fam, "", "  ")
	return out, nil
}

// flatAct wraps a flat script as the unnamed act it is (v2 §4.4).
func flatAct(loose []ast.Statement) *ast.Act {
	flat := &ast.Act{Body: &ast.Block{}}
	for _, s := range loose {
		if u, isUse := s.(*ast.Use); isUse {
			flat.Uses = append(flat.Uses, u.Module)
			continue
		}
		flat.Body.Statements = append(flat.Body.Statements, s)
	}
	return flat
}

func envelopeOne(a *ast.Act, h host.Host) ActEnvelope {
	out := ActEnvelope{Name: a.Name, Depends: a.Depends}
	env := &Envelope{}
	top := map[string]bool{}
	observes, mutates := false, false

	if a.Body != nil {
		for _, s := range a.Body.Statements {
			walkStatement(s, func(e ast.Expression) {
				v, isVerb := e.(*ast.Verb)
				if !isVerb {
					return
				}
				switch v.Verb {
				case token.READ:
					observes = true
					env.FSRead = append(env.FSRead, literalArg(v.Args, 0))
				case token.FETCH:
					observes = true
					env.Network = true
				case token.ASK:
					// The outer channel (D-040): it addresses the invoker,
					// not the world, so it has no envelope axis.
					observes = true
				case token.WRITE:
					mutates = true
					env.FSWrite = append(env.FSWrite, pathOf(v, token.TO, 0))
				case token.CREATE:
					mutates = true
					env.FSWrite = append(env.FSWrite, clauseLiteral(v, token.AT))
				case token.RUN:
					mutates = true
					if v.Shell != "" {
						env.Subprocess = append(env.Subprocess, "*")
						top["run"] = true
					}
					// `run tool <name>` reaches a module; its footprint
					// arrives through the use-grant below.
				case token.SEND:
					dest, has := sendDestination(v)
					switch {
					case !has:
						// Result form: reaches nothing outside the act.
					case dest == "output" || dest == "user":
						// The script's own answer channel.
					case strings.HasPrefix(dest, "http://"), strings.HasPrefix(dest, "https://"):
						mutates = true
						env.Network = true
					case dest == "*":
						// A computed destination could be a URL; module
						// destinations are already covered by their grant.
						mutates = true
						env.Network = true
						top["send"] = true
					default:
						// A named module destination — covered by its grant.
						mutates = true
					}
				}
			})
		}
	}

	// Modules contribute by grant, not by call site (D-055): a granted
	// module is reachable even where no call is written.
	uses := append([]string(nil), a.Uses...)
	sort.Strings(uses)
	for _, name := range uses {
		note := ModuleNote{Name: name}
		var effects []string
		if m, installed := h.Module(name); installed {
			if d, declares := m.(EffectDeclarer); declares {
				// nil is "undeclared" (projects to ⊤); an empty slice is a
				// declaration of purity (D-056).
				if eff := d.Effects(); eff != nil {
					effects = eff
					note.Declared = true
				}
			}
		}
		if !note.Declared {
			effects = []string{"subprocess", "network", "fs_read", "fs_write"}
			top["module "+name] = true
		}
		note.Effects = effects
		out.Modules = append(out.Modules, note)
		for _, eff := range effects {
			mutates = true
			switch eff {
			case "subprocess":
				env.Subprocess = append(env.Subprocess, moduleMark(name, note.Declared))
			case "network":
				env.Network = true
			case "fs_read":
				env.FSRead = append(env.FSRead, moduleMark(name, note.Declared))
			case "fs_write":
				env.FSWrite = append(env.FSWrite, moduleMark(name, note.Declared))
			}
		}
		if len(effects) == 0 {
			mutates = false || mutates // a declared-pure module adds nothing
		}
	}

	env.Subprocess = dedupe(env.Subprocess)
	env.FSRead = dedupe(env.FSRead)
	env.FSWrite = dedupe(env.FSWrite)
	out.Envelope = *env

	switch {
	case mutates:
		out.EffectLevel = "io"
	case observes:
		out.EffectLevel = "observe"
	default:
		out.EffectLevel = "pure"
	}

	for name := range top {
		out.Top = append(out.Top, name)
	}
	sort.Strings(out.Top)
	return out
}

// moduleMark labels a module's contribution to a path/subprocess axis: a
// declared module contributes under its own name, an undeclared one is ⊤.
func moduleMark(name string, declared bool) string {
	if declared {
		return "module:" + name
	}
	return "*"
}

// literalArg reads argument i as written, or "*" when it is computed.
func literalArg(args []ast.Expression, i int) string {
	if i < len(args) {
		if s, ok := args[i].(*ast.StringLiteral); ok {
			return s.Value
		}
	}
	return "*"
}

// pathOf prefers the clause form (`write <data> to <path>`) and falls back
// to the positional form (`write <path> <data>`).
func pathOf(v *ast.Verb, kw token.Type, argIndex int) string {
	if c, has := v.Clause(kw); has {
		if s, ok := c.(*ast.StringLiteral); ok {
			return s.Value
		}
		return "*"
	}
	return literalArg(v.Args, argIndex)
}

func clauseLiteral(v *ast.Verb, kw token.Type) string {
	if c, has := v.Clause(kw); has {
		if s, ok := c.(*ast.StringLiteral); ok {
			return s.Value
		}
	}
	return "*"
}

// sendDestination resolves a send's `to` clause as written: the literal
// text, "*" when computed, or absent for the result form.
func sendDestination(v *ast.Verb) (string, bool) {
	c, has := v.Clause(token.TO)
	if !has {
		return "", false
	}
	switch d := c.(type) {
	case *ast.StringLiteral:
		return d.Value, true
	case *ast.Identifier:
		return d.Value, true
	}
	return "*", true
}

func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
