package evaluator

import (
	"regexp"
	"sort"
	"strings"

	"github.com/typedmirror/mana/internal/ast"
	"github.com/typedmirror/mana/internal/host"
	"github.com/typedmirror/mana/internal/object"
	"github.com/typedmirror/mana/internal/token"
)

// knownTransforms is the whole data vocabulary, named so an unknown transform
// can say what does exist instead of failing blankly.
var knownTransforms = []string{
	"filter", "map", "sort", "group", "take", "count", "sum", "trim", "lowercase", "matches",
}

func (e *Evaluator) evalTransform(n *ast.Transform, input object.Value, sc *scope) object.Value {
	if n.Direction != "" && n.Name != "sort" {
		return e.fail(n, "%s does not take a direction — only sort does", n.Name)
	}
	switch n.Name {
	case "filter":
		return e.transformFilter(n, input, sc)
	case "map":
		return e.transformMap(n, input, sc)
	case "sort":
		return e.transformSort(n, input, sc)
	case "group":
		return e.transformGroup(n, input, sc)
	case "take":
		return e.transformTake(n, input, sc)
	case "count":
		return e.transformCount(n, input)
	case "sum":
		return e.transformSum(n, input)
	case "matches":
		return e.transformMatches(n, input, sc)
	case "trim", "lowercase":
		return e.transformString(n, input)
	}
	// A transform that is not built in gets one chance to be a module verb
	// before it is reported as unknown, so `@x -> search_web` works when the
	// act has used it.
	if e.uses[n.Name] {
		m, err := e.module(n, n.Name)
		if err != nil {
			return err
		}
		out := m.Execute(host.Call{Args: []object.Value{input}, Intent: e.currentIntent()})
		if out == nil {
			return e.fail(n, "module %q returned nothing", n.Name)
		}
		if bad, isErr := out.(*object.Err); isErr {
			return e.adopt(n, bad)
		}
		return out
	}
	return e.fail(n, "unknown transform %q — known transforms are %s", n.Name, strings.Join(knownTransforms, ", "))
}

// selector returns the per-element expression a transform operates with: its
// argument, or the value of a named clause. Presence is reported separately so
// `sort` with no key can still sort by the element itself.
func selector(n *ast.Transform, kw token.Type) (ast.Expression, bool) {
	if v, ok := n.Clause(kw); ok {
		return v, true
	}
	if n.Arg != nil {
		return n.Arg, true
	}
	return nil, false
}

// elements requires a list. Transforms are list operations, and quietly
// promoting a scalar to a one-element list would hide the shape mismatch that
// caused it.
func (e *Evaluator) elements(n *ast.Transform, input object.Value) ([]object.Value, *object.Err) {
	list, ok := input.(*object.List)
	if !ok {
		return nil, e.fail(n, "%s needs a list, got a %s", n.Name, input.Type())
	}
	return list.Elements, nil
}

// perElement evaluates expr once per element with that element as `@`.
func (e *Evaluator) perElement(expr ast.Expression, elems []object.Value, sc *scope) ([]object.Value, *object.Err) {
	out := make([]object.Value, 0, len(elems))
	for _, el := range elems {
		v := e.eval(expr, sc.withSelf(el))
		if bad, isErr := v.(*object.Err); isErr {
			return nil, bad
		}
		out = append(out, v)
	}
	return out, nil
}

func (e *Evaluator) transformFilter(n *ast.Transform, input object.Value, sc *scope) object.Value {
	cond, ok := selector(n, token.WHERE)
	if !ok {
		return e.fail(n, "filter needs a condition — `filter where <condition>`")
	}
	elems, err := e.elements(n, input)
	if err != nil {
		return err
	}
	keep, err := e.perElement(cond, elems, sc)
	if err != nil {
		return err
	}
	out := &object.List{}
	for i, k := range keep {
		if object.Truthy(k) {
			out.Elements = append(out.Elements, elems[i])
		}
	}
	return out
}

func (e *Evaluator) transformMap(n *ast.Transform, input object.Value, sc *scope) object.Value {
	expr := n.Arg
	if expr == nil {
		return e.fail(n, "map needs an expression — `map <field>` or `map { ... }`")
	}
	elems, err := e.elements(n, input)
	if err != nil {
		return err
	}
	mapped, err := e.perElement(expr, elems, sc)
	if err != nil {
		return err
	}
	return &object.List{Elements: mapped}
}

func (e *Evaluator) transformSort(n *ast.Transform, input object.Value, sc *scope) object.Value {
	elems, err := e.elements(n, input)
	if err != nil {
		return err
	}
	sorted := append([]object.Value(nil), elems...)

	// Keys are computed once up front rather than inside the comparator: a key
	// expression can fail, and a sort comparator has nowhere to report to.
	keys := sorted
	if key, hasKey := selector(n, token.BY); hasKey {
		computed, err := e.perElement(key, sorted, sc)
		if err != nil {
			return err
		}
		keys = computed
	}

	// Descending inverts the comparator rather than reversing the result, so
	// ties keep their input order either way.
	descending := n.Direction == "descending"
	idx := make([]int, len(sorted))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		c := object.Compare(keys[idx[a]], keys[idx[b]])
		if descending {
			return c > 0
		}
		return c < 0
	})
	out := make([]object.Value, len(sorted))
	for i, j := range idx {
		out[i] = sorted[j]
	}
	return &object.List{Elements: out}
}

// transformMatches tests a string against a regular expression.
func (e *Evaluator) transformMatches(n *ast.Transform, input object.Value, sc *scope) object.Value {
	if n.Arg == nil {
		return e.fail(n, `matches needs a pattern — `+"`matches \".+@.+\"`")
	}
	pv := e.eval(n.Arg, sc)
	if object.IsErr(pv) {
		return pv
	}
	pattern, ok := asText(pv)
	if !ok {
		return e.fail(n, "matches needs a pattern string, got a %s", pv.Type())
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return e.fail(n, "%q is not a valid pattern: %v", pattern, err)
	}
	switch x := input.(type) {
	case object.String:
		return object.Bool(re.MatchString(string(x)))
	case object.Word:
		return object.Bool(re.MatchString(string(x)))
	}
	return e.fail(n, "matches needs a string, got a %s", input.Type())
}

// transformGroup buckets elements by a key and returns one record per bucket:
// `{ key, count, items }`. The count field is why spec §14's
// `group by region |> sort by count |> take 5` works with no extra vocabulary.
func (e *Evaluator) transformGroup(n *ast.Transform, input object.Value, sc *scope) object.Value {
	key, ok := selector(n, token.BY)
	if !ok {
		return e.fail(n, "group needs a key — `group by <field>`")
	}
	elems, err := e.elements(n, input)
	if err != nil {
		return err
	}
	keys, err := e.perElement(key, elems, sc)
	if err != nil {
		return err
	}
	var order []string
	buckets := map[string]*object.List{}
	labels := map[string]object.Value{}
	for i, k := range keys {
		s := object.Text(k)
		if _, seen := buckets[s]; !seen {
			order = append(order, s)
			buckets[s] = &object.List{}
			labels[s] = k
		}
		buckets[s].Elements = append(buckets[s].Elements, elems[i])
	}
	out := &object.List{}
	for _, s := range order {
		r := object.NewRecord()
		r.Set("key", labels[s])
		r.Set("count", object.Number(len(buckets[s].Elements)))
		r.Set("items", buckets[s])
		out.Elements = append(out.Elements, r)
	}
	return out
}

func (e *Evaluator) transformTake(n *ast.Transform, input object.Value, sc *scope) object.Value {
	if n.Arg == nil {
		return e.fail(n, "take needs a count — `take 5`")
	}
	nv := e.eval(n.Arg, sc)
	if object.IsErr(nv) {
		return nv
	}
	count, ok := nv.(object.Number)
	if !ok || count < 0 {
		return e.fail(n, "take needs a non-negative number, got %s", nv.Inspect())
	}
	elems, err := e.elements(n, input)
	if err != nil {
		return err
	}
	if int(count) < len(elems) {
		elems = elems[:int(count)]
	}
	return &object.List{Elements: append([]object.Value(nil), elems...)}
}

func (e *Evaluator) transformCount(n *ast.Transform, input object.Value) object.Value {
	switch x := input.(type) {
	case *object.List:
		return object.Number(len(x.Elements))
	case *object.Record:
		return object.Number(len(x.Keys()))
	case object.String:
		return object.Number(len(x))
	}
	return e.fail(n, "count needs a list, record, or string, got a %s", input.Type())
}

func (e *Evaluator) transformSum(n *ast.Transform, input object.Value) object.Value {
	list, ok := input.(*object.List)
	if !ok {
		return e.fail(n, "sum needs a list, got a %s", input.Type())
	}
	total := object.Number(0)
	for _, el := range list.Elements {
		num, isNum := el.(object.Number)
		if !isNum {
			return e.fail(n, "sum needs numbers, found a %s", el.Type())
		}
		total += num
	}
	return total
}

// transformString applies a text transform to a string, or elementwise to a
// list of them, so `@names -> lowercase` reads the way it looks.
func (e *Evaluator) transformString(n *ast.Transform, input object.Value) object.Value {
	apply := strings.TrimSpace
	if n.Name == "lowercase" {
		apply = strings.ToLower
	}
	switch x := input.(type) {
	case object.String:
		return object.String(apply(string(x)))
	case object.Word:
		return object.String(apply(string(x)))
	case *object.List:
		out := &object.List{Elements: make([]object.Value, 0, len(x.Elements))}
		for _, el := range x.Elements {
			s, ok := el.(object.String)
			if !ok {
				return e.fail(n, "%s needs strings, found a %s", n.Name, el.Type())
			}
			out.Elements = append(out.Elements, object.String(apply(string(s))))
		}
		return out
	}
	return e.fail(n, "%s needs a string or a list of strings, got a %s", n.Name, input.Type())
}
