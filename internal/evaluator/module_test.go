package evaluator

import (
	"strings"
	"testing"

	"github.com/typedmirror/mana/internal/host"
	"github.com/typedmirror/mana/internal/object"
)

// recorder registers a module and captures every call made to it, so a test can
// assert on what the script asked the module to do rather than only on what
// came back.
func recorder(h *host.Fake, name string, clauses []string) *[]host.Call {
	var seen []host.Call
	fn := func(c host.Call) object.Value {
		seen = append(seen, c)
		return object.String(name + " ran")
	}
	if clauses == nil {
		h.Register(name, fn)
	} else {
		h.RegisterWithClauses(name, clauses, fn)
	}
	return &seen
}

// --- resolution --------------------------------------------------------------

func TestModuleAsVerb(t *testing.T) {
	h := host.NewFake()
	calls := recorder(h, "postgres", nil)
	v, _ := ok(t, h, "use postgres\n"+`@r = postgres query "SELECT 1"`)
	eq(t, v, "postgres ran")

	if len(*calls) != 1 {
		t.Fatalf("got %d calls", len(*calls))
	}
	c := (*calls)[0]
	if c.Target != "query" {
		t.Errorf("target: got %q, want %q", c.Target, "query")
	}
	if len(c.Args) != 1 || c.Args[0].Inspect() != "SELECT 1" {
		t.Errorf("args: got %v", c.Args)
	}
}

func TestModuleWithNoTarget(t *testing.T) {
	h := host.NewFake()
	calls := recorder(h, "search_web", nil)
	ok(t, h, "use search_web\n"+`@r = search_web "mana programming language"`)
	c := (*calls)[0]
	if c.Target != "" {
		t.Errorf("a leading string is an argument, not a target: %q", c.Target)
	}
	if len(c.Args) != 1 || c.Args[0].Inspect() != "mana programming language" {
		t.Errorf("args: got %v", c.Args)
	}
}

// TestUsingAModuleIsRequired is the permission boundary (v2 §7.2). Installed is
// not the same as reachable.
func TestUsingAModuleIsRequired(t *testing.T) {
	h := host.NewFake()
	recorder(h, "postgres", nil)
	err := bad(t, h, `@r = postgres query "SELECT 1"`)
	if !strings.Contains(err.Reason, `module "postgres" is not used in this act`) {
		t.Errorf("got %q", err.Reason)
	}
	if !strings.Contains(err.Suggestion, "use postgres") {
		t.Errorf("the error should name the fix: %q", err.Suggestion)
	}
}

func TestUnknownModuleIsReportedAtRuntimeNotParseTime(t *testing.T) {
	// D-013: the grammar is context-free, so an unknown verb is a runtime
	// error with a useful message rather than a syntax error.
	err := bad(t, nil, `@r = nosuchthing query "x"`)
	if !strings.Contains(err.Reason, `no module named "nosuchthing" is used in this act`) {
		t.Errorf("got %q", err.Reason)
	}
}

// --- clauses -----------------------------------------------------------------

func TestModuleDeclaredClause(t *testing.T) {
	h := host.NewFake()
	calls := recorder(h, "inventory", []string{"item"})
	ok(t, h, "use inventory\n"+`@s = inventory check item "widget-x"`)
	c := (*calls)[0]
	if c.Target != "check" {
		t.Errorf("target: got %q", c.Target)
	}
	if got := c.Clauses["item"]; got == nil || got.Inspect() != "widget-x" {
		t.Errorf("clauses: got %v", c.Clauses)
	}
}

// TestUndeclaredClauseIsRejected is the whole point of Clauses(): a module says
// what it accepts, and anything else fails with the vocabulary listed.
func TestUndeclaredClauseIsRejected(t *testing.T) {
	h := host.NewFake()
	recorder(h, "slack", []string{"channel"})
	err := bad(t, h, "use slack\n-- notify ops channel of deployment\n"+`send "deployed" to slack thread "42"`)

	if !strings.Contains(err.Reason, `module "slack" does not accept clause "thread"`) {
		t.Errorf("reason: %q", err.Reason)
	}
	if !strings.Contains(err.Suggestion, "valid clauses for slack:") || !strings.Contains(err.Suggestion, "channel") {
		t.Errorf("suggestion should list the vocabulary: %q", err.Suggestion)
	}
	if err.Intent != "notify ops channel of deployment" {
		t.Errorf("intent did not reach the clause error: %q", err.Intent)
	}
}

// TestNilClausesMeansBuiltInsOnly is the optionality that lets a plain tool and
// a clause-declaring module share one interface.
func TestNilClausesMeansBuiltInsOnly(t *testing.T) {
	h := host.NewFake()
	recorder(h, "plain", nil) // Clauses() returns nil

	// A built-in keyword is fine.
	ok(t, h, "use plain\n"+`@r = plain fetchit with key "v"`)

	// A custom one is not, because the module declared none.
	err := bad(t, h, "use plain\n"+`@r = plain fetchit custom "v"`)
	if !strings.Contains(err.Reason, `does not accept clause "custom"`) {
		t.Errorf("got %q", err.Reason)
	}
}

func TestBuiltInClausesAlwaysAccepted(t *testing.T) {
	h := host.NewFake()
	calls := recorder(h, "store", []string{})
	ok(t, h, "use store\n"+`@r = store put with name "a" from ./x.json`)
	c := (*calls)[0]
	if _, has := c.Clauses["with"]; !has {
		t.Errorf("built-in `with` did not reach the module: %v", c.Clauses)
	}
	if _, has := c.Clauses["from"]; !has {
		t.Errorf("built-in `from` did not reach the module: %v", c.Clauses)
	}
}

func TestMultiFieldWith(t *testing.T) {
	h := host.NewFake()
	calls := recorder(h, "orders", nil)
	ok(t, h, "use orders\n@item = \"widget\"\n@qty = 10\n"+
		`@r = orders create with item @item quantity @qty`)
	rec, isRec := (*calls)[0].Clauses["with"].(*object.Record)
	if !isRec {
		t.Fatalf("with should carry a record: %v", (*calls)[0].Clauses)
	}
	if len(rec.Keys()) != 2 {
		t.Fatalf("got %v", rec.Keys())
	}
	if v, _ := rec.Get("quantity"); v.Inspect() != "10" {
		t.Errorf("got %v", rec.Keys())
	}
}

// --- send to a module --------------------------------------------------------

func TestSendToModuleDestination(t *testing.T) {
	h := host.NewFake()
	calls := recorder(h, "slack", []string{"channel"})
	ok(t, h, "use slack\n"+`send "deployment complete" to slack channel "ops"`)

	c := (*calls)[0]
	if c.Target != "send" {
		t.Errorf("target: got %q", c.Target)
	}
	if len(c.Args) != 1 || c.Args[0].Inspect() != "deployment complete" {
		t.Errorf("args: got %v", c.Args)
	}
	if got := c.Clauses["channel"]; got == nil || got.Inspect() != "ops" {
		t.Errorf("clauses: got %v", c.Clauses)
	}
	if _, leaked := c.Clauses["to"]; leaked {
		t.Error("the destination itself should not be handed to the module as a clause")
	}
}

func TestSendToAnInstalledButUnusedModule(t *testing.T) {
	h := host.NewFake()
	recorder(h, "slack", []string{"channel"})
	err := bad(t, h, `send "hi" to slack channel "ops"`)
	if !strings.Contains(err.Reason, `module "slack" is not used in this act`) {
		t.Errorf("got %q", err.Reason)
	}
	if !strings.Contains(err.Suggestion, "use slack") {
		t.Errorf("got %q", err.Suggestion)
	}
}

// --- module failures ---------------------------------------------------------

func TestModuleFailureCarriesIntent(t *testing.T) {
	h := host.NewFake()
	h.Register("flaky", func(host.Call) object.Value { return host.Fail("upstream refused") })
	err := bad(t, h, "use flaky\n-- reaching for the upstream service\n@r = flaky go")
	if err.Reason != "upstream refused" {
		t.Errorf("reason: %q", err.Reason)
	}
	if err.Intent != "reaching for the upstream service" {
		t.Errorf("intent: %q", err.Intent)
	}
}

// TestIntentIsHandedToTheModule: v2 §7.3 says Execute receives the current
// `--` line automatically, which is what lets a module report the way the
// language does.
func TestIntentIsHandedToTheModule(t *testing.T) {
	h := host.NewFake()
	calls := recorder(h, "svc", nil)
	ok(t, h, "use svc\n-- the reasoning in force\n@r = svc ping")
	if got := (*calls)[0].Intent; got != "the reasoning in force" {
		t.Errorf("got %q", got)
	}
}

func TestModuleReturningNothingIsReported(t *testing.T) {
	h := host.NewFake()
	h.Register("silent", func(host.Call) object.Value { return nil })
	err := bad(t, h, "use silent\n@r = silent go")
	if !strings.Contains(err.Reason, "returned nothing") {
		t.Errorf("got %q", err.Reason)
	}
}

// --- module in a pipe stage --------------------------------------------------

func TestModuleAsAPipeStage(t *testing.T) {
	h := host.NewFake()
	calls := recorder(h, "index", []string{"into"})
	ok(t, h, "use index\n"+`@r = [1, 2] |> index docs into "primary"`)
	c := (*calls)[0]
	if c.Target != "docs" {
		t.Errorf("target: got %q", c.Target)
	}
	if len(c.Args) != 1 || c.Args[0].Inspect() != "[1, 2]" {
		t.Errorf("the piped value should arrive as the first argument: %v", c.Args)
	}
}

func TestModuleAsATransformFallback(t *testing.T) {
	h := host.NewFake()
	calls := recorder(h, "enrich", nil)
	ok(t, h, "use enrich\n"+`@r = "seed" -> enrich`)
	if len(*calls) != 1 || (*calls)[0].Args[0].Inspect() != "seed" {
		t.Fatalf("got %v", *calls)
	}
}

func TestBuiltInTransformWinsOverAModuleOfTheSameName(t *testing.T) {
	// A module cannot shadow the data vocabulary; otherwise `use count` would
	// silently change what every existing script means.
	h := host.NewFake()
	recorder(h, "count", nil)
	v, _ := ok(t, h, "use count\n"+`@n = [1, 2, 3] |> count`)
	eq(t, v, "3")
}

// --- discovery ---------------------------------------------------------------

func TestAskToolsListsTheRegistry(t *testing.T) {
	h := host.NewFake()
	recorder(h, "slack", nil)
	recorder(h, "postgres", nil)
	v, _ := ok(t, h, "@tools = ask tools")
	// Sorted, and listing what is installed rather than what is used: this is
	// discovery, not access.
	eq(t, v, "[postgres, slack]")
}
