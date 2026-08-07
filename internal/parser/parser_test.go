package parser

import (
	"strings"
	"testing"
)

// parse is the happy path: it fails the test if the source did not parse
// cleanly, because a parser that reports success while dropping errors is worse
// than one that crashes.
func parse(t *testing.T, src string) string {
	t.Helper()
	p := New(src)
	prog := p.Parse()
	if errs := p.Errors(); len(errs) > 0 {
		t.Fatalf("parse errors:\n  %s", strings.Join(errs, "\n  "))
	}
	return prog.String()
}

func parseErr(t *testing.T, src string) []string {
	t.Helper()
	p := New(src)
	p.Parse()
	errs := p.Errors()
	if len(errs) == 0 {
		t.Fatalf("expected a parse error, got none")
	}
	return errs
}

func eq(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("got:\n  %s\nwant:\n  %s", got, want)
	}
}

func TestBindingAndIntent(t *testing.T) {
	eq(t, parse(t, "-- pulling user data\n@count = 42"), "-- pulling user data\n@count = 42")
}

func TestIntentIsAStatementNotAComment(t *testing.T) {
	// Spec §3: the `--` lines are not optional decoration. If the parser
	// dropped them the intent stack would have nothing to report at failure.
	out := parse(t, "-- one\n@x = 1\n-- two\n@y = 2")
	if strings.Count(out, "--") != 2 {
		t.Fatalf("intent lines were dropped: %q", out)
	}
}

func TestPrecedence(t *testing.T) {
	cases := []struct{ src, want string }{
		{"@n = 1 + 2 * 3", "@n = (1 + (2 * 3))"},
		{"@n = 10 - 3", "@n = (10 - 3)"},
		// Spec §14 writes this and means (count) / (count), not count / @users.
		{"@r = @active -> count / @users -> count", "@r = ((@active -> count) / (@users -> count))"},
		{"@c = @input -> trim -> lowercase", "@c = ((@input -> trim) -> lowercase)"},
		{"@s = @a + \"x\" + @b", "@s = ((@a + \"x\") + @b)"},
		{"@m = @a.b.c", "@m = @a.b.c"},
	}
	for _, c := range cases {
		eq(t, parse(t, c.src), c.want)
	}
}

func TestVerbsArgumentsAndClauses(t *testing.T) {
	cases := []struct{ src, want string }{
		{`fetch users from "https://api.com" where role is "admin"`,
			`fetch users from "https://api.com" where (role is "admin")`},
		{`create file at ./report.md with @content`, `create file at ./report.md with @content`},
		{`create user with name "alice"`, `create user with { name: "alice" }`},
		{`write @data to ./output.csv as csv`, `write @data to ./output.csv as csv`},
		{`write ./output.json @data`, `write ./output.json @data`},
		{`send @result to output`, `send @result to output`},
		{`ask "should I proceed?"`, `ask "should I proceed?"`},
		{`fetch https://api.example.com/health`, `fetch https://api.example.com/health`},
		{`read ./config.json`, `read ./config.json`},
	}
	for _, c := range cases {
		eq(t, parse(t, c.src), c.want)
	}
}

// TestRunTakesRawShell is spec §5.2: "No escaping, no subprocess wrapper."
// Flags and paths must survive unlexed.
func TestRunTakesRawShell(t *testing.T) {
	eq(t, parse(t, "run gcc -o /tmp/test /tmp/test.c"), "run gcc -o /tmp/test /tmp/test.c")
	eq(t, parse(t, "@r = run curl -s http://localhost:3000/health"),
		"@r = run curl -s http://localhost:3000/health")
	// §11: a named tool is not shell, so it parses normally.
	eq(t, parse(t, `@r = run tool search_web with query "mana language spec"`),
		`@r = run tool search_web with { query: "mana language spec" }`)
}

func TestRunWithNoCommandIsAnError(t *testing.T) {
	errs := parseErr(t, "run\n")
	if !strings.Contains(errs[0], "run needs a command") {
		t.Errorf("got %q", errs[0])
	}
}

func TestPipeChainAcrossLines(t *testing.T) {
	src := `@data
  |> filter where status is "active"
  |> map -> { name: @.name }
  |> sort by name
  |> send to output`
	eq(t, parse(t, src),
		`((((@data |> filter where (status is "active")) |> map { name: @.name }) |> sort by name) |> send to output)`)
}

func TestFallbackChainAcrossLines(t *testing.T) {
	src := `@config = read ./config.json
       or read ./config.default.json
       or { port: 8080 }`
	// Left-associative. For fallback the grouping is unobservable — "first
	// success wins" either way — so the standard Pratt shape is kept.
	eq(t, parse(t, src),
		"@config = ((read ./config.json or read ./config.default.json) or { port: 8080 })")
}

// TestFallbackIsNotSwallowedByTheVerb guards the one precedence mistake that
// would silently change meaning: `or` belongs to the binding, not to fetch's
// argument list.
func TestFallbackIsNotSwallowedByTheVerb(t *testing.T) {
	eq(t, parse(t, `@u = fetch user 1 or create user with role "basic"`),
		`@u = (fetch user 1 or create user with { role: "basic" })`)
}

func TestRecordsBothSeparators(t *testing.T) {
	// Spec §9 writes commas on one line and bare line breaks across several.
	eq(t, parse(t, `@u = { name: "alice", role: "admin" }`), `@u = { name: "alice", role: "admin" }`)
	src := `@config = {
    server: { port: 8080, host: "localhost" }
    features: ["auth", "logging"]
}`
	eq(t, parse(t, src),
		`@config = { server: { port: 8080, host: "localhost" }, features: ["auth", "logging"] }`)
}

func TestMatchArms(t *testing.T) {
	src := `@response |> match {
    ok data: send data to output
    err msg: ask "failed"
}`
	eq(t, parse(t, src), `(@response |> match { ok data: send data to output err msg: ask "failed" })`)
}

func TestMatchArmWithMultiStatementBody(t *testing.T) {
	// Spec §5.2's example: an arm body holds an intent line and two statements.
	src := `@result |> match {
    ok value: send value to output
    err code: -- build failed, inspect source
              @src = read /tmp/test.c
              ask "review?"
}`
	out := parse(t, src)
	if !strings.Contains(out, "-- build failed, inspect source") {
		t.Errorf("intent inside an arm body was dropped: %s", out)
	}
	if !strings.Contains(out, "@src = read /tmp/test.c; ask \"review?\"") {
		t.Errorf("arm body did not collect both statements: %s", out)
	}
}

func TestMatchWildcardBinder(t *testing.T) {
	eq(t, parse(t, "@d |> match { ok _: send 1 to output\nerr _: send 2 to output }"),
		"(@d |> match { ok _: send 1 to output err _: send 2 to output })")
}

func TestEmptyMatchArmIsAnError(t *testing.T) {
	errs := parseErr(t, "@d |> match { ok x:\nerr y: send 1 to output }")
	if !strings.Contains(strings.Join(errs, "\n"), "empty body") {
		t.Errorf("got %v", errs)
	}
}

func TestInlineConditional(t *testing.T) {
	eq(t, parse(t, `@mode = if @count > 100 then "batch" else "single"`),
		`@mode = if (@count > 100) then "batch" else "single"`)
}

func TestSelfReference(t *testing.T) {
	eq(t, parse(t, `@cards = @users -> map { display: @.name + " <" + @.email + ">" }`),
		`@cards = (@users -> map { display: (((@.name + " <") + @.email) + ">") })`)
}

func TestUnterminatedStringIsReported(t *testing.T) {
	errs := parseErr(t, `@x = "no closing quote`)
	if !strings.Contains(errs[0], "unexpected character") {
		t.Errorf("got %q", errs[0])
	}
}

func TestParserCollectsErrorsWithoutHanging(t *testing.T) {
	// A bad token must produce an error and progress, never a spin.
	p := New("@x = = =\n@y = 1")
	prog := p.Parse()
	if len(p.Errors()) == 0 {
		t.Fatal("expected errors")
	}
	if len(prog.Statements) == 0 {
		t.Fatal("parser gave up entirely instead of recovering")
	}
}

// TestSpecCompleteExample parses spec §14 verbatim. If the reference program in
// the spec does not parse, the spec and the implementation disagree.
func TestSpecCompleteExample(t *testing.T) {
	src := `-- daily user report script
-- pulling active users and generating summary for stakeholders

@endpoint = "https://api.company.com/v2"

-- get all users, fall back to yesterday's cache if API is down
@users = fetch @endpoint + "/users" or read ./cache/users.json

-- filter to active users with recent logins
@active = @users
  |> filter where last_login > "2025-01-01"
  |> filter where status is "active"

-- build report structure
@report = {
    date: context.env.today
    total_users: @users -> count
    active_users: @active -> count
    active_rate: @active -> count / @users -> count
    top_regions: @active |> group by region |> sort by count |> take 5
}

-- write locally first
write @report to ./reports/daily.json

-- check if the dashboard is running before pushing
@dash = run curl -s http://localhost:3000/health
@dash |> match {
    ok _:   -- dashboard up, push the report
            send @report to https://localhost:3000/api/reports
    err _:  -- dashboard down, notify
            ask "dashboard is offline. send report by email instead?"
}

-- done
send "daily report complete: " + @report.active_users + " active" to output`

	p := New(src)
	prog := p.Parse()
	if errs := p.Errors(); len(errs) > 0 {
		t.Fatalf("spec §14 does not parse:\n  %s", strings.Join(errs, "\n  "))
	}
	if got, want := len(prog.Statements), 16; got != want {
		t.Errorf("got %d statements, want %d:\n%s", got, want, prog.String())
	}
}

// --- v1 gap fixes ------------------------------------------------------------

// TestSortDirection covers gap 1.2: `sort by count descending`. Full words, so
// the syntax stays on tokens an LLM already emits.
func TestSortDirection(t *testing.T) {
	cases := []struct{ src, want string }{
		{"@t = @d |> sort by count descending", "@t = (@d |> sort by count descending)"},
		{"@t = @d |> sort by name ascending", "@t = (@d |> sort by name ascending)"},
		{"@t = @d |> sort descending", "@t = (@d |> sort descending)"},
		{"@t = @d |> sort by name", "@t = (@d |> sort by name)"},
	}
	for _, c := range cases {
		eq(t, parse(t, c.src), c.want)
	}
}

func TestSortDirectionThenAnotherStage(t *testing.T) {
	eq(t, parse(t, "@t = @d |> sort by count descending |> take 5"),
		"@t = ((@d |> sort by count descending) |> take 5)")
}

// TestComparisonOperators covers gap 1.3.
func TestComparisonOperators(t *testing.T) {
	cases := []struct{ src, want string }{
		{"@b = @n >= 5", "@b = (@n >= 5)"},
		{"@b = @n <= 5", "@b = (@n <= 5)"},
		{"@b = @n == 5", "@b = (@n == 5)"},
		{"@b = @n != 5", "@b = (@n != 5)"},
		{"@b = @n is 5", "@b = (@n is 5)"},
		// Multiplicative before additive before comparison.
		{"@b = 1 + 2 * 3 > 4", "@b = ((1 + (2 * 3)) > 4)"},
	}
	for _, c := range cases {
		eq(t, parse(t, c.src), c.want)
	}
}

func TestBareLoneBangIsAnError(t *testing.T) {
	errs := parseErr(t, "@x = !5")
	if !strings.Contains(errs[0], "unexpected character") {
		t.Errorf("got %q", errs[0])
	}
}

// TestBarePathsAndURLs covers gap 1.4: the spec writes both unquoted.
func TestBarePathsAndURLs(t *testing.T) {
	cases := []struct{ src, want string }{
		{"@x = read ./config.json", "@x = read ./config.json"},
		{"@x = read ../up/one.json", "@x = read ../up/one.json"},
		{"@x = read /abs/path.json", "@x = read /abs/path.json"},
		{"@x = fetch https://api.com/v2/users", "@x = fetch https://api.com/v2/users"},
		{"@x = fetch http://localhost:3000/health", "@x = fetch http://localhost:3000/health"},
	}
	for _, c := range cases {
		eq(t, parse(t, c.src), c.want)
	}
}

func TestNestedMatch(t *testing.T) {
	src := `@a |> match {
    ok x: @b |> match {
              ok y: send y to output
              err e: send e to output
          }
    err m: send m to output
}`
	out := parse(t, src)
	if strings.Count(out, "match {") != 2 {
		t.Errorf("the inner match was not parsed: %s", out)
	}
}

func TestPipeChainWithManyStages(t *testing.T) {
	eq(t, parse(t, "@o = @d |> filter where active |> map name |> sort |> take 3 |> count"),
		"@o = (((((@d |> filter where active) |> map name) |> sort) |> take 3) |> count)")
}

func TestLongFallbackChain(t *testing.T) {
	eq(t, parse(t, "@c = read ./a or read ./b or read ./c or { port: 1 }"),
		"@c = (((read ./a or read ./b) or read ./c) or { port: 1 })")
}

func TestMatchesTransform(t *testing.T) {
	eq(t, parse(t, `@ok = @email -> matches ".+@.+"`), `@ok = (@email -> matches ".+@.+")`)
}

// TestTransformArgumentDoesNotSwallowTheChain: `map name -> trim` is two
// stages. Parsed with a looser argument precedence, `map` absorbed the rest of
// the chain and a whole stage disappeared without any error.
func TestTransformArgumentDoesNotSwallowTheChain(t *testing.T) {
	eq(t, parse(t, "@o = @users -> map name -> trim -> lowercase"),
		"@o = (((@users -> map name) -> trim) -> lowercase)")
	eq(t, parse(t, "@o = @users |> map name |> sort"),
		"@o = ((@users |> map name) |> sort)")
}

// --- error paths and remaining productions -----------------------------------

func TestGroupingAndPrefixMinus(t *testing.T) {
	eq(t, parse(t, "@n = (1 + 2) * 3"), "@n = ((1 + 2) * 3)")
	eq(t, parse(t, "@n = -5"), "@n = (-5)")
	eq(t, parse(t, "@n = 0 - -5"), "@n = (0 - (-5))")
}

func TestKeywordsAreLegalFieldNames(t *testing.T) {
	// After a dot, `to` is a field name, not a clause.
	eq(t, parse(t, "@x = context.to"), "@x = context.to")
	eq(t, parse(t, "@x = { from: 1 }"), "@x = { from: 1 }")
}

func TestRecordErrors(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{`@x = { 5: 1 }`, "expected a field name in a record"},
		{`@x = { a 1 }`, "expected :"},
		{`@x = { a: 1 b: 2 }`, "expected ',' or a line break"},
	} {
		errs := parseErr(t, c.src)
		if !strings.Contains(strings.Join(errs, "\n"), c.want) {
			t.Errorf("%s: got %v, want %q", c.src, errs, c.want)
		}
	}
}

func TestListErrors(t *testing.T) {
	errs := parseErr(t, "@x = [1 2]")
	if !strings.Contains(strings.Join(errs, "\n"), "expected ',' or a line break") {
		t.Errorf("got %v", errs)
	}
}

func TestListAcrossLines(t *testing.T) {
	eq(t, parse(t, "@x = [\n  1\n  2\n]"), "@x = [1, 2]")
}

func TestIfErrors(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{`@x = if true "a" else "b"`, "expected THEN"},
		{`@x = if true then "a" "b"`, "expected ELSE"},
	} {
		errs := parseErr(t, c.src)
		if !strings.Contains(strings.Join(errs, "\n"), c.want) {
			t.Errorf("%s: got %v", c.src, errs)
		}
	}
}

func TestMatchErrors(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{"@d |> match ok x: send 1 to output", "expected {"},
		{"@d |> match { }", "match needs at least one arm"},
		{"@d |> match { nonsense }", "expected a match arm"},
		{"@d |> match { ok x: send 1 to output", "expected }"},
	} {
		errs := parseErr(t, c.src)
		if !strings.Contains(strings.Join(errs, "\n"), c.want) {
			t.Errorf("%s: got %v, want %q", c.src, errs, c.want)
		}
	}
}

func TestUnclosedParenIsReported(t *testing.T) {
	errs := parseErr(t, "@x = (1 + 2")
	if !strings.Contains(strings.Join(errs, "\n"), "expected )") {
		t.Errorf("got %v", errs)
	}
}

// TestPipeIntoALiteralIsAnError: a stage has to be something that acts.
func TestPipeIntoALiteralIsAnError(t *testing.T) {
	errs := parseErr(t, "@x = [1] |> 5")
	if !strings.Contains(strings.Join(errs, "\n"), "cannot be a pipe stage") {
		t.Errorf("got %v", errs)
	}
}

func TestTrailingGarbageAfterAStatement(t *testing.T) {
	errs := parseErr(t, "@x = 1 )")
	if !strings.Contains(strings.Join(errs, "\n"), "after statement") {
		t.Errorf("got %v", errs)
	}
}

func TestWithClauseFormsAreDistinguished(t *testing.T) {
	// A bare name plus a value is a one-field record; anything else is an
	// ordinary expression.
	eq(t, parse(t, `create user with name "alice"`), `create user with { name: "alice" }`)
	eq(t, parse(t, `create user with @data`), `create user with @data`)
	eq(t, parse(t, `create user with { a: 1 }`), `create user with { a: 1 }`)
}
