package evaluator

import (
	"fmt"
	"strings"

	"github.com/typedmirror/mana/internal/ast"
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
	out, err := e.host.Run(n.Shell)
	if err != nil {
		return e.adopt(n, &object.Err{Reason: err.Error()})
	}
	if out.Code != 0 {
		reason := fmt.Sprintf("exit %d", out.Code)
		if msg := strings.TrimSpace(out.Stderr); msg != "" {
			reason += ": " + msg
		}
		return e.adopt(n, &object.Err{Reason: reason})
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
	out, err := e.host.CallTool(name, args)
	if err != nil {
		return e.adopt(n, &object.Err{
			Reason:     err.Error(),
			Suggestion: "check available tools with `ask tools`",
		})
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

// verbSend emits a result. `to` is required: a send with no destination is the
// kind of statement that looks like it did something and did not.
func (e *Evaluator) verbSend(n *ast.Verb, sc *scope, piped object.Value) object.Value {
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
		return e.fail(n, "send needs a destination — `send <value> to output`")
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
	case strings.HasPrefix(dest, "http://"), strings.HasPrefix(dest, "https://"):
		body, err := e.host.Post(dest, object.JSON(data))
		if err != nil {
			return e.adopt(n, &object.Err{Reason: err.Error()})
		}
		return decode(body)
	}
	return e.adopt(n, &object.Err{
		Reason:     fmt.Sprintf("no destination named %q", dest),
		Suggestion: "destinations are `output`, `user`, or an http(s) URL",
	})
}

// --- ask ---------------------------------------------------------------------

// verbAsk prompts. `ask tools` is the one reserved form: spec §11 uses it to
// enumerate what the environment has bound.
func (e *Evaluator) verbAsk(n *ast.Verb, sc *scope, piped object.Value) object.Value {
	if len(n.Args) == 1 {
		if id, ok := n.Args[0].(*ast.Identifier); ok && id.Value == "tools" {
			out := &object.List{}
			for _, name := range e.host.ToolNames() {
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
