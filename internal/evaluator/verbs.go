package evaluator

import (
	"fmt"
	"strings"

	"github.com/typedmirror/mana/internal/ast"
	"github.com/typedmirror/mana/internal/host"
	"github.com/typedmirror/mana/internal/object"
	"github.com/typedmirror/mana/internal/token"
)

// evalVerb executes one of the seven primitives (spec §5). piped is the value
// arriving from `|>`, or nil when the verb starts a statement.
func (e *Evaluator) evalVerb(n *ast.Verb, sc *scope, piped object.Value) object.Value {
	switch n.Verb {
	case token.FETCH:
		return e.verbFetch(n, sc)
	case token.READ:
		return e.verbRead(n, sc)
	case token.RUN:
		return e.verbRun(n, sc)
	case token.CREATE:
		return e.verbCreate(n, sc)
	case token.WRITE:
		return e.verbWrite(n, sc, piped)
	case token.SEND:
		return e.verbSend(n, sc, piped)
	case token.ASK:
		return e.verbAsk(n, sc, piped)
	}
	return e.fail(n, "unknown verb %q", n.Tok.Literal)
}

// --- argument plumbing -------------------------------------------------------

// clauseValue evaluates a clause if present.
func (e *Evaluator) clauseValue(n *ast.Verb, kw token.Type, sc *scope) (object.Value, bool) {
	expr, ok := n.Clause(kw)
	if !ok {
		return nil, false
	}
	return e.eval(expr, sc), true
}

// argValue evaluates the nth positional argument if it exists.
func (e *Evaluator) argValue(n *ast.Verb, i int, sc *scope) (object.Value, bool) {
	if i >= len(n.Args) {
		return nil, false
	}
	return e.eval(n.Args[i], sc), true
}

// target resolves the thing a verb acts on: a named clause first, then the last
// positional argument. Spec §6 says clauses resolve by keyword and never by
// position, so the clause always wins.
func (e *Evaluator) target(n *ast.Verb, kw token.Type, argIndex int, sc *scope) (object.Value, bool) {
	if v, ok := e.clauseValue(n, kw, sc); ok {
		return v, true
	}
	return e.argValue(n, argIndex, sc)
}

// asText renders a value that names a location — a path, a URL, a destination.
func asText(v object.Value) (string, bool) {
	switch x := v.(type) {
	case object.String:
		return string(x), true
	case object.Word:
		return string(x), true
	}
	return "", false
}

// --- fetch -------------------------------------------------------------------

// verbFetch retrieves over HTTP. `fetch users from URL where role is "admin"`
// reads as one sentence and runs as three steps: fetch, pick the `users` field
// out of the response if there is one, then filter.
func (e *Evaluator) verbFetch(n *ast.Verb, sc *scope) object.Value {
	src, ok := e.target(n, token.FROM, 0, sc)
	if !ok {
		return e.fail(n, "fetch needs a URL")
	}
	if object.IsErr(src) {
		return src
	}
	url, ok := asText(src)
	if !ok {
		return e.fail(n, "fetch needs a URL, got a %s", src.Type())
	}

	e.observe("fetch: %s", url)
	body, err := e.host.Fetch(url)
	if err != nil {
		return e.adopt(n, &object.Err{
			Reason:     err.Error(),
			Suggestion: "retry with backoff, or fall back with `or read ./cache/...`",
		})
	}
	result := decode(body)

	// A named target selects a field when the response wraps its payload.
	if _, fromClause := n.Clause(token.FROM); fromClause && len(n.Args) > 0 {
		if name, isIdent := n.Args[0].(*ast.Identifier); isIdent {
			if rec, isRec := result.(*object.Record); isRec {
				if v, has := rec.Get(name.Value); has {
					result = v
				}
			}
		}
	}
	return e.applyWhere(n, result, sc)
}

// applyWhere runs a verb's `where` clause over a list result (spec §6).
func (e *Evaluator) applyWhere(n *ast.Verb, result object.Value, sc *scope) object.Value {
	cond, ok := n.Clause(token.WHERE)
	if !ok {
		return result
	}
	list, isList := result.(*object.List)
	if !isList {
		return e.fail(n, "`where` needs a list to filter, got a %s", result.Type())
	}
	out := &object.List{}
	for _, el := range list.Elements {
		keep := e.eval(cond, sc.withSelf(el))
		if object.IsErr(keep) {
			return keep
		}
		if object.Truthy(keep) {
			out.Elements = append(out.Elements, el)
		}
	}
	return out
}

// decode turns a response body into values. JSON becomes structure; anything
// else stays text, so a plain-text endpoint is usable without ceremony.
func decode(body string) object.Value {
	if v, err := object.ParseJSON(body); err == nil {
		return v
	}
	return object.String(body)
}

// --- read --------------------------------------------------------------------

func (e *Evaluator) verbRead(n *ast.Verb, sc *scope) object.Value {
	src, ok := e.target(n, token.FROM, 0, sc)
	if !ok {
		return e.fail(n, "read needs a path")
	}
	if object.IsErr(src) {
		return src
	}
	path, ok := asText(src)
	if !ok {
		return e.fail(n, "read needs a path, got a %s", src.Type())
	}
	e.observe("read: %s", path)
	body, err := e.host.ReadFile(path)
	if err != nil {
		return e.adopt(n, &object.Err{
			Reason:     err.Error(),
			Suggestion: "check the path, or supply a default with `or { ... }`",
		})
	}

	format, _ := e.formatOf(n, path, sc)
	if format == "json" {
		v, jerr := object.ParseJSON(body)
		if jerr != nil {
			return e.fail(n, "%s is not valid JSON: %v", path, jerr)
		}
		return v
	}
	return object.String(body)
}

// formatOf resolves an `as` clause, falling back to the file extension. The
// extension is only a default; an explicit `as` always wins.
func (e *Evaluator) formatOf(n *ast.Verb, path string, sc *scope) (string, bool) {
	if v, ok := e.clauseValue(n, token.AS, sc); ok {
		if s, isText := asText(v); isText {
			return strings.ToLower(s), true
		}
	}
	switch {
	case strings.HasSuffix(path, ".json"):
		return "json", false
	case strings.HasSuffix(path, ".csv"):
		return "csv", false
	}
	return "text", false
}

// --- run ---------------------------------------------------------------------

// verbRun executes shell, or calls an ambient tool when written as
// `run tool <name> with { ... }` (spec §11).
func (e *Evaluator) verbRun(n *ast.Verb, sc *scope) object.Value {
	if n.Shell == "" {
		return e.runTool(n, sc)
	}
	e.effect("run: %s", n.Shell)
	out, err := e.host.Run(n.Shell, e.timeout)
	if err != nil {
		return e.adopt(n, &object.Err{Reason: err.Error()})
	}
	if out.Truncated {
		e.note("run %q: output was truncated at %d KB", n.Shell, host.MaxOutputBytes>>10)
	}
	if out.TimedOut {
		return e.adopt(n, &object.Err{
			Reason:     fmt.Sprintf("timed out after %s", e.runTimeout()),
			Suggestion: "raise the limit with --timeout, or background it: `run " + n.Shell + " > /dev/null 2>&1 &`",
		})
	}
	if out.Code != 0 {
		reason := fmt.Sprintf("exit %d", out.Code)
		if msg := strings.TrimSpace(out.Stderr); msg != "" {
			reason += ": " + msg
		}
		return e.adopt(n, &object.Err{Reason: reason})
	}
	// A command can succeed and still say something. The value stays the
	// stdout string, because that is what a script wants to work with — but
	// dropping stderr entirely would be silent loss, so it is recorded.
	if msg := strings.TrimSpace(out.Stderr); msg != "" {
		e.note("run %q: exit 0 with stderr: %s", n.Shell, msg)
	}
	return object.String(strings.TrimRight(out.Stdout, "\n"))
}

func (e *Evaluator) runTool(n *ast.Verb, sc *scope) object.Value {
	if len(n.Args) < 2 {
		return e.fail(n, "run needs a shell command, or `run tool <name> with { ... }`")
	}
	nameVal := e.eval(n.Args[1], sc)
	if object.IsErr(nameVal) {
		return nameVal
	}
	name, ok := asText(nameVal)
	if !ok {
		return e.fail(n, "a tool name must be a word, got a %s", nameVal.Type())
	}
	var args object.Value = object.Null{}
	if v, has := e.clauseValue(n, token.WITH, sc); has {
		if object.IsErr(v) {
			return v
		}
		args = v
	}
	m, bad := e.module(n, name)
	if bad != nil {
		return bad
	}
	call := host.Call{Intent: e.currentIntent(), Clauses: map[string]object.Value{}}
	if rec, isRec := args.(*object.Record); isRec {
		for _, k := range rec.Keys() {
			v, _ := rec.Get(k)
			call.Clauses[k] = v
		}
	} else if _, isNull := args.(object.Null); !isNull {
		call.Args = append(call.Args, args)
	}
	e.effect("tool %s", name)
	out := m.Execute(call)
	if out == nil {
		return e.fail(n, "module %q returned nothing", name)
	}
	if err, isErr := out.(*object.Err); isErr {
		return e.adopt(n, err)
	}
	return out
}

// --- create ------------------------------------------------------------------

// verbCreate makes a new resource. Only files have a provider here; anything
// else fails loudly rather than reporting a creation that did not happen.
func (e *Evaluator) verbCreate(n *ast.Verb, sc *scope) object.Value {
	kind := "resource"
	if len(n.Args) > 0 {
		if id, ok := n.Args[0].(*ast.Identifier); ok {
			kind = id.Value
		}
	}
	if kind != "file" {
		return e.adopt(n, &object.Err{
			Reason:     fmt.Sprintf("no provider is bound for creating a %q", kind),
			Suggestion: "`create file at <path> with <content>` is the only provider in v0.1",
		})
	}
	at, ok := e.clauseValue(n, token.AT, sc)
	if !ok {
		return e.fail(n, "create file needs `at <path>`")
	}
	if object.IsErr(at) {
		return at
	}
	path, ok := asText(at)
	if !ok {
		return e.fail(n, "create file needs a path, got a %s", at.Type())
	}
	content, ok := e.clauseValue(n, token.WITH, sc)
	if !ok {
		return e.fail(n, "create file needs `with <content>`")
	}
	if object.IsErr(content) {
		return content
	}
	e.effect("create file: %s", path)
	if err := e.host.WriteFile(path, object.Text(content)); err != nil {
		return e.adopt(n, &object.Err{Reason: err.Error()})
	}
	return object.String(path)
}

// --- write -------------------------------------------------------------------

// verbWrite persists a value. Spec §5 writes it both ways — `write <path>
// <data>` and `write <data> to <path>` — so both are accepted, with the clause
// deciding which argument is which.
func (e *Evaluator) verbWrite(n *ast.Verb, sc *scope, piped object.Value) object.Value {
	var pathVal, data object.Value
	if v, ok := e.clauseValue(n, token.TO, sc); ok {
		pathVal = v
		data = piped
		if data == nil {
			if a, has := e.argValue(n, 0, sc); has {
				data = a
			}
		}
	} else {
		a, ok := e.argValue(n, 0, sc)
		if !ok {
			return e.fail(n, "write needs a path")
		}
		pathVal = a
		data = piped
		if data == nil {
			if b, has := e.argValue(n, 1, sc); has {
				data = b
			}
		}
	}
	if data == nil {
		return e.fail(n, "write needs something to write")
	}
	if object.IsErr(pathVal) {
		return pathVal
	}
	if object.IsErr(data) {
		return data
	}
	path, ok := asText(pathVal)
	if !ok {
		return e.fail(n, "write needs a path, got a %s", pathVal.Type())
	}

	format, _ := e.formatOf(n, path, sc)
	body, ferr := render(format, data)
	if ferr != nil {
		return e.adopt(n, ferr)
	}
	e.effect("write: %s", path)
	if err := e.host.WriteFile(path, body); err != nil {
		return e.adopt(n, &object.Err{Reason: err.Error()})
	}
	return object.String(path)
}

// render serializes a value in the requested format.
func render(format string, v object.Value) (string, *object.Err) {
	switch format {
	case "json":
		return object.JSON(v), nil
	case "csv":
		return object.CSV(v)
	}
	return object.Text(v), nil
}

// --- send --------------------------------------------------------------------

// verbSend emits a result, or — inside an act body, with no destination — sets
// that act's result (v2 §4.6).
func (e *Evaluator) verbSend(n *ast.Verb, sc *scope, piped object.Value) object.Value {
	// `send err <reason>` fails the enclosing act. Checked before arguments are
	// evaluated, because `err` is a marker here rather than a value.
	if id, isErr := sendErrMarker(n); isErr {
		return e.sendErr(n, id, sc)
	}

	data := piped
	if data == nil {
		v, ok := e.argValue(n, 0, sc)
		if !ok {
			return e.fail(n, "send needs a value")
		}
		data = v
	}
	if object.IsErr(data) {
		return data
	}
	destVal, ok := e.clauseValue(n, token.TO, sc)
	if !ok {
		return e.sendWithoutDestination(n, data, sc)
	}
	if object.IsErr(destVal) {
		return destVal
	}
	dest, ok := asText(destVal)
	if !ok {
		return e.fail(n, "send needs a destination, got a %s", destVal.Type())
	}

	switch {
	case dest == "output" || dest == "user":
		fmt.Fprintln(e.host.Out(), object.Text(data))
		return object.Null{}
	case e.uses[dest]:
		// `send @x to slack channel "ops"` — a destination naming a used module
		// hands the value to it, with the module's own clauses attached.
		return e.sendToModule(n, dest, data, sc)
	case strings.HasPrefix(dest, "http://"), strings.HasPrefix(dest, "https://"):
		e.effect("send to %s", dest)
		body, err := e.host.Post(dest, object.JSON(data))
		if err != nil {
			return e.adopt(n, &object.Err{Reason: err.Error()})
		}
		return decode(body)
	}
	if _, installed := e.host.Module(dest); installed {
		return e.adopt(n, &object.Err{
			Reason:     fmt.Sprintf("module %q is not used in this act", dest),
			Suggestion: fmt.Sprintf("add `use %s` to the act", dest),
		})
	}
	return e.adopt(n, &object.Err{
		Reason:     fmt.Sprintf("no destination named %q", dest),
		Suggestion: "destinations are `output`, `user`, an http(s) URL, or a used module",
	})
}

// sendToModule delivers a value to a module destination (v2 §7.1).
func (e *Evaluator) sendToModule(n *ast.Verb, name string, data object.Value, sc *scope) object.Value {
	m, bad := e.module(n, name)
	if bad != nil {
		return bad
	}
	call := host.Call{
		Target:  "send",
		Args:    []object.Value{data},
		Clauses: map[string]object.Value{},
		Intent:  e.currentIntent(),
	}
	for _, c := range n.Clauses {
		if c.Kw == token.TO {
			continue // the destination itself, already resolved
		}
		if err := e.checkClause(n, m, c); err != nil {
			return err
		}
		v := e.eval(c.Value, sc)
		if object.IsErr(v) {
			return v
		}
		call.Clauses[c.Name()] = v
	}
	e.effect("send to module %s", name)
	out := m.Execute(call)
	if out == nil {
		return object.Null{}
	}
	if err, isErr := out.(*object.Err); isErr {
		return e.adopt(n, err)
	}
	return out
}

// --- ask ---------------------------------------------------------------------

// verbAsk prompts. `ask tools` is the one reserved form: spec §11 uses it to
// enumerate what the environment has bound.
func (e *Evaluator) verbAsk(n *ast.Verb, sc *scope, piped object.Value) object.Value {
	if len(n.Args) == 1 {
		if id, ok := n.Args[0].(*ast.Identifier); ok && id.Value == "tools" {
			out := &object.List{}
			for _, name := range e.host.Modules() {
				out.Elements = append(out.Elements, object.Word(name))
			}
			return out
		}
	}
	prompt := piped
	if prompt == nil {
		v, ok := e.argValue(n, 0, sc)
		if !ok {
			return e.fail(n, "ask needs a prompt")
		}
		prompt = v
	}
	if object.IsErr(prompt) {
		return prompt
	}
	answer, err := e.host.Ask(object.Text(prompt))
	if err != nil {
		return e.adopt(n, &object.Err{Reason: err.Error()})
	}
	return object.String(answer)
}

// sendErrMarker reports whether a send is the `send err <reason>` form. The
// marker is a bare `err` as the first argument, which is why it is detected on
// the syntax rather than on an evaluated value.
func sendErrMarker(n *ast.Verb) (*ast.Identifier, bool) {
	if len(n.Args) < 1 {
		return nil, false
	}
	if _, hasTo := n.Clause(token.TO); hasTo {
		return nil, false
	}
	id, ok := n.Args[0].(*ast.Identifier)
	if !ok || id.Value != "err" {
		return nil, false
	}
	return id, true
}

// sendErr fails the enclosing act with a stated reason (v2 §4.6).
func (e *Evaluator) sendErr(n *ast.Verb, marker *ast.Identifier, sc *scope) object.Value {
	if !e.inAct {
		return e.fail(n, "`send err` sets an act's failure and is only meaningful inside an act")
	}
	if len(n.Args) < 2 {
		return e.fail(n, "`send err` needs a reason — `send err \"what went wrong\"`")
	}
	reason := e.eval(n.Args[1], sc)
	if object.IsErr(reason) {
		return reason
	}
	_ = marker
	return e.adopt(n, &object.Err{Reason: object.Text(reason)})
}

// sendWithoutDestination handles a `send` with no `to` clause.
//
// Inside an act that is the act's result (v2 §4.6). Outside one it stays an
// error, deliberately: a send with nowhere to go looks like it did something
// and did not, and outside an act there is no result for it to become.
func (e *Evaluator) sendWithoutDestination(n *ast.Verb, data object.Value, sc *scope) object.Value {
	if !e.inAct {
		return e.fail(n, "send needs a destination — `send <value> to output`")
	}
	return e.setResult(n, data)
}
