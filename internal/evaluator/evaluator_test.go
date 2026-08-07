package evaluator

import (
	"strings"
	"testing"

	"github.com/typedmirror/mana/internal/host"
	"github.com/typedmirror/mana/internal/object"
	"github.com/typedmirror/mana/internal/parser"
)

// run executes src against a Fake host and returns the last value plus the host,
// so a test can assert on the effects as well as the result.
func run(t *testing.T, h *host.Fake, src string) (object.Value, *host.Fake) {
	t.Helper()
	if h == nil {
		h = host.NewFake()
	}
	p := parser.New(src)
	prog := p.Parse()
	if errs := p.Errors(); len(errs) > 0 {
		t.Fatalf("parse errors:\n  %s", strings.Join(errs, "\n  "))
	}
	return New(h).Run(prog), h
}

// ok asserts the script succeeded and returns the value.
func ok(t *testing.T, h *host.Fake, src string) (object.Value, *host.Fake) {
	t.Helper()
	v, h := run(t, h, src)
	if err, bad := v.(*object.Err); bad {
		t.Fatalf("unexpected failure: %s", err.Reason)
	}
	return v, h
}

// bad asserts the script failed and returns the error.
func bad(t *testing.T, h *host.Fake, src string) *object.Err {
	t.Helper()
	v, _ := run(t, h, src)
	err, isErr := v.(*object.Err)
	if !isErr {
		t.Fatalf("expected a failure, got %s(%s)", v.Type(), v.Inspect())
	}
	return err
}

func eq(t *testing.T, got object.Value, want string) {
	t.Helper()
	if got.Inspect() != want {
		t.Errorf("got %q, want %q", got.Inspect(), want)
	}
}

// --- literals and operators --------------------------------------------------

func TestLiteralsAndArithmetic(t *testing.T) {
	cases := []struct{ src, want string }{
		{"@x = 42", "42"},
		{"@x = 3.14", "3.14"},
		{"@x = 1 + 2 * 3", "7"},
		{"@x = 10 - 3", "7"},
		{"@x = true", "true"},
		{`@x = "alice"`, "alice"},
		{"@x = [1, 2, 3]", "[1, 2, 3]"},
		{`@x = { name: "alice", role: "admin" }`, "{ name: alice, role: admin }"},
		{`@x = "a" + 1`, "a1"},
		{`@x = 2 is 2`, "true"},
		{`@x = "2025-06-01" > "2025-01-01"`, "true"},
	}
	for _, c := range cases {
		v, _ := ok(t, nil, c.src)
		eq(t, v, c.want)
	}
}

func TestDivisionByZeroIsAnError(t *testing.T) {
	// +Inf in a report is a silent failure wearing a number.
	if got := bad(t, nil, "@x = 1 / 0").Reason; !strings.Contains(got, "division by zero") {
		t.Errorf("got %q", got)
	}
}

func TestUnboundReferenceIsAnError(t *testing.T) {
	if got := bad(t, nil, "@y = @nope + 1").Reason; !strings.Contains(got, "@nope is not bound") {
		t.Errorf("got %q", got)
	}
}

func TestInlineConditional(t *testing.T) {
	v, _ := ok(t, nil, `@count = 200
@mode = if @count > 100 then "batch" else "single"`)
	eq(t, v, "batch")
}

// --- the intent channel ------------------------------------------------------

// TestIntentReachesTheError is the language's central claim (spec §10.1): the
// reasoning that preceded a failure is attached to it automatically.
func TestIntentReachesTheError(t *testing.T) {
	err := bad(t, nil, `-- pulling user data for the monthly report
@users = fetch https://api.example.com/users`)
	if err.Intent != "pulling user data for the monthly report" {
		t.Errorf("intent not attached: %q", err.Intent)
	}
	if !strings.Contains(err.Reason, "connection refused") {
		t.Errorf("reason lost: %q", err.Reason)
	}
	if !strings.Contains(err.At, "fetch") {
		t.Errorf("operation lost: %q", err.At)
	}
}

func TestMostRecentIntentWins(t *testing.T) {
	err := bad(t, nil, `-- first thing
@a = 1
-- second thing, the one that matters
@b = read ./missing.json`)
	if err.Intent != "second thing, the one that matters" {
		t.Errorf("got %q", err.Intent)
	}
}

func TestIntentStackIsPreserved(t *testing.T) {
	h := host.NewFake()
	p := parser.New("-- one\n-- two\n@x = 1")
	e := New(h)
	e.Run(p.Parse())
	if got := e.Intents(); len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Errorf("got %v", got)
	}
}

func TestErrorRendersAsTheSpecStructure(t *testing.T) {
	// Spec §10.1 pins the shape of an error value.
	out := bad(t, nil, "-- why\n@x = read ./nope.json").Inspect()
	for _, field := range []string{"status: err", "at:", "intent:", "reason:", "suggestion:"} {
		if !strings.Contains(out, field) {
			t.Errorf("error value is missing %q:\n%s", field, out)
		}
	}
}

// --- errors as data ----------------------------------------------------------

func TestFallbackCatchesFailure(t *testing.T) {
	h := host.NewFake()
	h.Files["./config.default.json"] = `{"port": 9090}`
	v, _ := ok(t, h, `@config = read ./config.json
       or read ./config.default.json
       or { port: 8080 }`)
	eq(t, v, "{ port: 9090 }")
}

func TestFallbackReachesTheLastOption(t *testing.T) {
	v, _ := ok(t, nil, "@config = read ./a.json or read ./b.json or { port: 8080 }")
	eq(t, v, "{ port: 8080 }")
}

func TestMatchDispatchesOnOkAndErr(t *testing.T) {
	h := host.NewFake()
	h.Responses["https://api.example.com/users"] = `[{"name":"alice"}]`
	_, h = ok(t, h, `@data = fetch https://api.example.com/users
@data |> match {
    ok users: send users to output
    err msg:  send "failed" to output
}`)
	if got := h.Stdout.String(); !strings.Contains(got, "alice") {
		t.Errorf("ok arm did not run: %q", got)
	}
}

func TestMatchErrArmBindsTheReason(t *testing.T) {
	_, h := ok(t, nil, `@data = fetch https://down.example.com/users
@data |> match {
    ok users: send users to output
    err msg:  send "request failed: " + msg to output
}`)
	if got := h.Stdout.String(); !strings.Contains(got, "request failed: connection refused") {
		t.Errorf("err arm did not bind the reason: %q", got)
	}
}

// TestUnmatchedErrorSurvives is the no-silent-swallow rule: a match with only an
// ok arm must not turn a failure into a pass.
func TestUnmatchedErrorSurvives(t *testing.T) {
	err := bad(t, nil, `@data = fetch https://down.example.com/x
@data |> match { ok users: send users to output }`)
	if !strings.Contains(err.Reason, "connection refused") {
		t.Errorf("error was swallowed by a partial match: %q", err.Reason)
	}
}

// TestBrokenChainStopsInsteadOfContinuing guards the other half: a failed stage
// must not feed garbage into the next one.
func TestBrokenChainStopsInsteadOfContinuing(t *testing.T) {
	err := bad(t, nil, `@x = fetch https://down.example.com/x |> count`)
	if !strings.Contains(err.Reason, "connection refused") {
		t.Errorf("got %q — the failure should survive the pipe", err.Reason)
	}
}

func TestWildcardBinderDiscardsTheValue(t *testing.T) {
	_, h := ok(t, nil, `@d = fetch https://down.example.com/x
@d |> match {
    ok _:  send "up" to output
    err _: send "down" to output
}`)
	if got := strings.TrimSpace(h.Stdout.String()); got != "down" {
		t.Errorf("got %q", got)
	}
}

// --- verbs -------------------------------------------------------------------

func TestFetchParsesJSONAndFiltersWithWhere(t *testing.T) {
	h := host.NewFake()
	h.Responses["https://api.com"] = `{"users":[{"name":"a","role":"admin"},{"name":"b","role":"basic"}]}`
	v, _ := ok(t, h, `@x = fetch users from "https://api.com" where role is "admin"`)
	eq(t, v, `[{ name: a, role: admin }]`)
}

func TestFetchOfPlainTextStaysText(t *testing.T) {
	h := host.NewFake()
	h.Responses["https://api.example.com/health"] = "OK"
	v, _ := ok(t, h, "@x = fetch https://api.example.com/health")
	eq(t, v, "OK")
}

func TestReadParsesJSONByExtension(t *testing.T) {
	h := host.NewFake()
	h.Files["./config.json"] = `{"port": 8080, "mode": "debug"}`
	v, _ := ok(t, h, "@c = read ./config.json")
	eq(t, v, "{ port: 8080, mode: debug }")
}

func TestReadOfMalformedJSONFailsLoudly(t *testing.T) {
	h := host.NewFake()
	h.Files["./config.json"] = `{not json`
	if got := bad(t, h, "@c = read ./config.json").Reason; !strings.Contains(got, "not valid JSON") {
		t.Errorf("got %q", got)
	}
}

func TestRunCapturesStdout(t *testing.T) {
	h := host.NewFake()
	h.Shells["curl -s http://localhost:3000/health"] = host.Shell{Stdout: "ok\n"}
	v, _ := ok(t, h, "@dash = run curl -s http://localhost:3000/health")
	eq(t, v, "ok")
}

// TestRunNonZeroExitIsAnError is spec §5.2's err arm: a failed build is a
// failure, not an empty string.
func TestRunNonZeroExitIsAnError(t *testing.T) {
	h := host.NewFake()
	h.Shells["gcc -o /tmp/test /tmp/test.c"] = host.Shell{Code: 1, Stderr: "undefined reference"}
	err := bad(t, h, "run gcc -o /tmp/test /tmp/test.c")
	if !strings.Contains(err.Reason, "exit 1") || !strings.Contains(err.Reason, "undefined reference") {
		t.Errorf("got %q", err.Reason)
	}
}

func TestWriteBothArgumentOrders(t *testing.T) {
	_, h := ok(t, nil, `@data = { a: 1 }
write ./out.json @data`)
	if len(h.Written) != 1 || h.Written[0].Path != "./out.json" || h.Written[0].Content != `{"a":1}` {
		t.Fatalf("got %+v", h.Written)
	}

	_, h2 := ok(t, nil, `@data = { a: 1 }
write @data to ./other.json`)
	if len(h2.Written) != 1 || h2.Written[0].Path != "./other.json" {
		t.Fatalf("got %+v", h2.Written)
	}
}

func TestWriteAsCSV(t *testing.T) {
	_, h := ok(t, nil, `@rows = [{ name: "a", n: 1 }, { name: "b", n: 2 }]
write @rows to ./out.csv as csv`)
	if got, want := h.Written[0].Content, "name,n\na,1\nb,2"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSendToOutput(t *testing.T) {
	_, h := ok(t, nil, `send "hello" to output`)
	if got := strings.TrimSpace(h.Stdout.String()); got != "hello" {
		t.Errorf("got %q", got)
	}
}

func TestSendToURLPostsJSON(t *testing.T) {
	h := host.NewFake()
	h.Responses["https://localhost:3000/api/reports"] = `{"ok":true}`
	_, h = ok(t, h, `@r = { total: 2 }
send @r to https://localhost:3000/api/reports`)
	if len(h.Posted) != 1 || h.Posted[0].Body != `{"total":2}` {
		t.Fatalf("got %+v", h.Posted)
	}
}

// TestSendToAnUnknownDestinationFails keeps `send` from looking like it
// delivered something when nothing was bound to receive it.
func TestSendToAnUnknownDestinationFails(t *testing.T) {
	if got := bad(t, nil, `send "x" to slack`).Reason; !strings.Contains(got, "no destination named") {
		t.Errorf("got %q", got)
	}
}

func TestAskPromptsAndReturnsTheAnswer(t *testing.T) {
	h := host.NewFake()
	h.Answers = []string{"yes"}
	v, h := ok(t, h, `@a = ask "should I proceed?"`)
	eq(t, v, "yes")
	if len(h.Asked) != 1 || h.Asked[0] != "should I proceed?" {
		t.Errorf("got %v", h.Asked)
	}
}

func TestCreateFile(t *testing.T) {
	_, h := ok(t, nil, `@c = "# report"
create file at ./report.md with @c`)
	if len(h.Written) != 1 || h.Written[0].Content != "# report" {
		t.Fatalf("got %+v", h.Written)
	}
}

// TestCreateWithNoProviderFailsLoudly: `create user with name "alice"` parses,
// but nothing can satisfy it here, and reporting success would be a lie.
func TestCreateWithNoProviderFailsLoudly(t *testing.T) {
	err := bad(t, nil, `create user with name "alice"`)
	if !strings.Contains(err.Reason, `no provider is bound for creating a "user"`) {
		t.Errorf("got %q", err.Reason)
	}
}

// --- tools (spec §11) --------------------------------------------------------

func TestAskToolsListsAmbientTools(t *testing.T) {
	h := host.NewFake()
	h.Register("search_web", func(host.Call) object.Value { return object.String("hits") })
	v, _ := ok(t, h, "@tools = ask tools")
	eq(t, v, "[search_web]")
}

func TestRunToolCallsIt(t *testing.T) {
	h := host.NewFake()
	var got object.Value
	h.Register("search_web", func(c host.Call) object.Value {
		got = c.Clauses["query"]
		return object.String("hits")
	})
	v, _ := ok(t, h, "use search_web\n"+`@r = run tool search_web with query "mana language spec"`)
	eq(t, v, "hits")
	// `with query "…"` arrives as a named clause, not as an opaque record — the
	// module sees the keyword the script wrote.
	eq(t, got, "mana language spec")
}

func TestUnknownToolFailsWithASuggestion(t *testing.T) {
	err := bad(t, nil, `@r = run tool nope with query "x"`)
	if !strings.Contains(err.Reason, `no module named "nope" is used`) {
		t.Errorf("got %+v", err)
	}
}

// TestRunToolStillNeedsUse: tools and modules are one registry, so `run tool X`
// goes through the same permission gate as `X …`. Without this, an act that
// declined to `use postgres` could still reach it through `run tool postgres`,
// and the use boundary would be decorative.
func TestRunToolStillNeedsUse(t *testing.T) {
	h := host.NewFake()
	h.Register("search_web", func(host.Call) object.Value { return object.String("hits") })
	err := bad(t, h, `@r = run tool search_web with query "x"`)
	if !strings.Contains(err.Reason, "is not used in this act") {
		t.Errorf("got %q", err.Reason)
	}
	if !strings.Contains(err.Suggestion, "use search_web") {
		t.Errorf("the error should name the fix: %q", err.Suggestion)
	}
}

// --- transforms --------------------------------------------------------------

func TestTransformChain(t *testing.T) {
	src := `@users = [
    { name: "alice", region: "eu", active: true },
    { name: "bob", region: "us", active: false },
    { name: "cara", region: "eu", active: true }
]
@out = @users |> filter where active |> map name |> sort`
	v, _ := ok(t, nil, src)
	eq(t, v, "[alice, cara]")
}

// TestGroupSortTake pins sort as ascending and stable.
//
// SPEC GAP: §14 calls `group by region |> sort by count |> take 5` the
// "top_regions", which needs descending order, but the spec defines no
// direction and no `desc` keyword. Ascending is the documented behaviour until
// the spec grows one; inventing a keyword here would put the two out of sync.
func TestGroupSortTake(t *testing.T) {
	src := `@users = [
    { region: "eu" }, { region: "us" }, { region: "eu" }, { region: "ap" }
]
@ranked = @users |> group by region |> sort by count |> map key`
	v, _ := ok(t, nil, src)
	eq(t, v, "[us, ap, eu]")
}

func TestSelfReferenceInMap(t *testing.T) {
	src := `@users = [{ name: "alice", email: "a@x" }]
@cards = @users -> map { display: @.name + " <" + @.email + ">" }`
	v, _ := ok(t, nil, src)
	eq(t, v, "[{ display: alice <a@x> }]")
}

func TestCountAndSum(t *testing.T) {
	v, _ := ok(t, nil, "@n = [1, 2, 3] -> sum")
	eq(t, v, "6")
	v, _ = ok(t, nil, "@n = [1, 2, 3] -> count")
	eq(t, v, "3")
}

func TestStringTransformChain(t *testing.T) {
	v, _ := ok(t, nil, `@clean = "  HeLLo  " -> trim -> lowercase`)
	eq(t, v, "hello")
}

// TestMisspelledFieldInAFilterIsAnError is the quiet-wrong-answer guard: an
// unknown field inside a transform must fail, not silently filter everything
// out and report an empty list as a successful run.
func TestMisspelledFieldInAFilterIsAnError(t *testing.T) {
	src := `@users = [{ status: "active" }]
@out = @users |> filter where stauts is "active"`
	if got := bad(t, nil, src).Reason; !strings.Contains(got, `no field "stauts"`) {
		t.Errorf("got %q", got)
	}
}

func TestUnknownTransformNamesTheKnownOnes(t *testing.T) {
	err := bad(t, nil, "@x = [1, 2] |> flatten")
	if !strings.Contains(err.Reason, `unknown transform "flatten"`) || !strings.Contains(err.Reason, "filter") {
		t.Errorf("got %q", err.Reason)
	}
}

func TestTransformOnAScalarFailsRatherThanPromoting(t *testing.T) {
	if got := bad(t, nil, "@x = 5 |> count").Reason; !strings.Contains(got, "count needs a list") {
		t.Errorf("got %q", got)
	}
	if got := bad(t, nil, "@x = 5 |> sum").Reason; !strings.Contains(got, "sum needs a list") {
		t.Errorf("got %q", got)
	}
}

// --- context (spec §12) ------------------------------------------------------

func TestContextIsAmbient(t *testing.T) {
	v, _ := ok(t, nil, "@x = context.env.os")
	eq(t, v, "testos")
	v, _ = ok(t, nil, "@x = context.user")
	eq(t, v, "tester")
	v, _ = ok(t, nil, "@x = context.messages -> count")
	eq(t, v, "2")
}

func TestUnknownContextFieldFails(t *testing.T) {
	if got := bad(t, nil, "@x = context.nope").Reason; !strings.Contains(got, `no field "nope"`) {
		t.Errorf("got %q", got)
	}
}

// --- the spec's own program --------------------------------------------------

// TestSpecCompleteExampleRuns executes spec §14 end to end. It is the strongest
// available check that the spec and the implementation agree: if the reference
// program cannot run, one of the two is wrong.
func TestSpecCompleteExampleRuns(t *testing.T) {
	h := host.NewFake()
	h.Responses["https://api.company.com/v2/users"] = `[
        {"name":"alice","region":"eu","status":"active","last_login":"2025-06-01"},
        {"name":"bob","region":"us","status":"inactive","last_login":"2024-02-01"},
        {"name":"cara","region":"eu","status":"active","last_login":"2025-09-01"}
    ]`
	h.Shells["curl -s http://localhost:3000/health"] = host.Shell{Stdout: "ok"}
	h.Responses["https://localhost:3000/api/reports"] = `{"received":true}`

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
    top_regions: @active |> group by region |> sort by count descending |> take 5
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

	v, h := ok(t, h, src)
	if object.IsErr(v) {
		t.Fatalf("spec §14 failed: %s", v.Inspect())
	}
	if len(h.Written) != 1 || h.Written[0].Path != "./reports/daily.json" {
		t.Fatalf("report was not written: %+v", h.Written)
	}
	if !strings.Contains(h.Written[0].Content, `"active_users":2`) {
		t.Errorf("wrong report body: %s", h.Written[0].Content)
	}
	if !strings.Contains(h.Written[0].Content, `"date":"2026-08-07"`) {
		t.Errorf("context.env.today did not resolve: %s", h.Written[0].Content)
	}
	if len(h.Posted) != 1 {
		t.Errorf("the ok arm did not push the report: %+v", h.Posted)
	}
	if got := strings.TrimSpace(h.Stdout.String()); got != "daily report complete: 2 active" {
		t.Errorf("final line: %q", got)
	}
}

// TestSpecCompleteExampleWithTheAPIDown exercises the same program's failure
// path: fallback to cache, and the err arm of the match.
func TestSpecCompleteExampleWithTheAPIDown(t *testing.T) {
	h := host.NewFake()
	h.Files["./cache/users.json"] = `[{"name":"alice","region":"eu","status":"active","last_login":"2025-06-01"}]`
	h.Answers = []string{"yes"}

	src := `-- get all users, fall back to yesterday's cache if API is down
@users = fetch "https://api.company.com/v2/users" or read ./cache/users.json
@active = @users |> filter where status is "active"
@dash = run curl -s http://localhost:3000/health
@dash |> match {
    ok _:   send @active to https://localhost:3000/api/reports
    err _:  ask "dashboard is offline. send report by email instead?"
}`
	v, h := ok(t, h, src)
	eq(t, v, "yes")
	if len(h.Posted) != 0 {
		t.Errorf("the ok arm ran even though the shell command failed: %+v", h.Posted)
	}
	if len(h.Asked) != 1 {
		t.Errorf("the err arm did not run: %v", h.Asked)
	}
}

// --- v1 gap fixes ------------------------------------------------------------

// TestSortDescending covers gap 1.2. Spec §14's `top_regions` needs this to
// mean what its name says.
func TestSortDescending(t *testing.T) {
	src := `@users = [
    { region: "eu" }, { region: "us" }, { region: "eu" }, { region: "ap" }, { region: "eu" }
]
@top = @users |> group by region |> sort by count descending |> take 1 |> map key`
	v, _ := ok(t, nil, src)
	eq(t, v, "[eu]")
}

func TestSortAscendingIsTheDefault(t *testing.T) {
	a, _ := ok(t, nil, "@x = [3, 1, 2] |> sort")
	eq(t, a, "[1, 2, 3]")
	b, _ := ok(t, nil, "@x = [3, 1, 2] |> sort ascending")
	eq(t, b, "[1, 2, 3]")
	c, _ := ok(t, nil, "@x = [3, 1, 2] |> sort descending")
	eq(t, c, "[3, 2, 1]")
}

// TestDescendingKeepsTiesStable: descending inverts the comparator rather than
// reversing the output, so equal keys hold their input order either way.
func TestDescendingKeepsTiesStable(t *testing.T) {
	src := `@d = [{ k: 1, tag: "first" }, { k: 1, tag: "second" }, { k: 2, tag: "third" }]
@o = @d |> sort by k descending |> map tag`
	v, _ := ok(t, nil, src)
	eq(t, v, "[third, first, second]")
}

func TestDirectionOnANonSortIsAnError(t *testing.T) {
	if got := bad(t, nil, "@x = [1, 2] |> take 1 descending").Reason; !strings.Contains(got, "only sort does") {
		t.Errorf("got %q", got)
	}
}

// TestComparisonOperators covers gap 1.3.
func TestComparisonOperators(t *testing.T) {
	cases := []struct{ src, want string }{
		{"@b = 5 >= 5", "true"},
		{"@b = 4 >= 5", "false"},
		{"@b = 5 <= 5", "true"},
		{"@b = 6 <= 5", "false"},
		{"@b = 5 == 5", "true"},
		{"@b = 5 != 5", "false"},
		{"@b = 5 != 6", "true"},
		{`@b = "a" == "a"`, "true"},
		{"@b = [1, 2] == [1, 2]", "true"},
		{"@b = { a: 1 } != { a: 2 }", "true"},
	}
	for _, c := range cases {
		v, _ := ok(t, nil, c.src)
		eq(t, v, c.want)
	}
}

func TestIsAndDoubleEqualsAgree(t *testing.T) {
	a, _ := ok(t, nil, `@b = "admin" is "admin"`)
	c, _ := ok(t, nil, `@b = "admin" == "admin"`)
	if a.Inspect() != c.Inspect() {
		t.Errorf("`is` and `==` disagree: %s vs %s", a.Inspect(), c.Inspect())
	}
}

func TestComparisonInAFilter(t *testing.T) {
	src := `@d = [{ n: 1 }, { n: 5 }, { n: 9 }]
@o = @d |> filter where n >= 5 |> map n`
	v, _ := ok(t, nil, src)
	eq(t, v, "[5, 9]")
}

// TestMatchesTransform covers the `matches` built-in.
func TestMatchesTransform(t *testing.T) {
	v, _ := ok(t, nil, `@ok = "a@b.com" -> matches ".+@.+"`)
	eq(t, v, "true")
	v, _ = ok(t, nil, `@ok = "nope" -> matches ".+@.+"`)
	eq(t, v, "false")
}

func TestMatchesRejectsABadPattern(t *testing.T) {
	if got := bad(t, nil, `@x = "a" -> matches "("`).Reason; !strings.Contains(got, "not a valid pattern") {
		t.Errorf("got %q", got)
	}
}

func TestMatchesNeedsAString(t *testing.T) {
	if got := bad(t, nil, `@x = [1] -> matches "a"`).Reason; !strings.Contains(got, "matches needs a string") {
		t.Errorf("got %q", got)
	}
}

func TestMatchesInAFilter(t *testing.T) {
	src := `@d = [{ email: "a@b.com" }, { email: "broken" }]
@o = @d |> filter where email -> matches ".+@.+" |> count`
	v, _ := ok(t, nil, src)
	eq(t, v, "1")
}

// TestUppercaseIsGone covers gap 1.6: it was never in the spec, so it is not in
// the implementation.
func TestUppercaseIsGone(t *testing.T) {
	err := bad(t, nil, `@x = "a" -> uppercase`)
	if !strings.Contains(err.Reason, `unknown transform "uppercase"`) {
		t.Errorf("got %q", err.Reason)
	}
	if strings.Contains(err.Reason, "uppercase,") {
		t.Errorf("uppercase is still advertised as known: %q", err.Reason)
	}
}

// --- error model, spec §10.1 -------------------------------------------------

// TestErrorStructureFieldByField checks each field the spec names, rather than
// only that the rendering contains the words.
func TestErrorStructureFieldByField(t *testing.T) {
	h := host.NewFake()
	err := bad(t, h, `-- pulling user data for report
@u = fetch https://api.com/users`)

	if err.Type() != "err" {
		t.Errorf("status: got %q, want %q", err.Type(), "err")
	}
	if err.At != "fetch https://api.com/users" {
		t.Errorf("at: got %q", err.At)
	}
	if err.Intent != "pulling user data for report" {
		t.Errorf("intent: got %q", err.Intent)
	}
	if err.Reason != "connection refused" {
		t.Errorf("reason: got %q", err.Reason)
	}
	if err.Line != 2 {
		t.Errorf("line: got %d, want 2", err.Line)
	}
}

func TestErrorWithNoPrecedingIntentStillReportsTheField(t *testing.T) {
	// A blank intent means nothing was reasoned before the failure, which is
	// itself worth seeing — so the field is present and empty, not omitted.
	err := bad(t, nil, "@u = read ./nope.json")
	if err.Intent != "" {
		t.Errorf("got %q", err.Intent)
	}
	if !strings.Contains(err.Inspect(), `intent: ""`) {
		t.Errorf("the intent field was omitted:\n%s", err.Inspect())
	}
}

func TestErrorInsideATransformCarriesIntent(t *testing.T) {
	src := `-- filtering on a field that does not exist
@d = [{ a: 1 }]
@o = @d |> filter where nope is 1`
	err := bad(t, nil, src)
	if err.Intent != "filtering on a field that does not exist" {
		t.Errorf("got %q", err.Intent)
	}
}

// --- operator failure paths --------------------------------------------------

func TestPrefixMinus(t *testing.T) {
	v, _ := ok(t, nil, "@x = 0 - -5")
	eq(t, v, "5")
	v, _ = ok(t, nil, "@x = -3 + 10")
	eq(t, v, "7")
}

func TestNegatingANonNumberIsAnError(t *testing.T) {
	if got := bad(t, nil, `@x = -"abc"`).Reason; !strings.Contains(got, "cannot negate a string") {
		t.Errorf("got %q", got)
	}
}

func TestNegatingAFailedExpressionPropagates(t *testing.T) {
	if got := bad(t, nil, "@x = -@nope").Reason; !strings.Contains(got, "not bound") {
		t.Errorf("got %q", got)
	}
}

func TestAddRejectsCollections(t *testing.T) {
	for _, src := range []string{`@x = [1] + "a"`, `@x = "a" + [1]`, `@x = { a: 1 } + 1`, `@x = 1 + { a: 1 }`} {
		if got := bad(t, nil, src).Reason; !strings.Contains(got, "cannot add") {
			t.Errorf("%s: got %q", src, got)
		}
	}
}

func TestArithmeticRejectsNonNumbers(t *testing.T) {
	for _, src := range []string{`@x = "a" - 1`, `@x = "a" * 2`, `@x = "a" / 2`} {
		if got := bad(t, nil, src).Reason; !strings.Contains(got, "cannot apply") {
			t.Errorf("%s: got %q", src, got)
		}
	}
}

func TestOrderRejectsCollections(t *testing.T) {
	for _, src := range []string{"@x = [1] > 1", "@x = 1 < [1]", "@x = { a: 1 } > 1"} {
		if got := bad(t, nil, src).Reason; !strings.Contains(got, "cannot order") {
			t.Errorf("%s: got %q", src, got)
		}
	}
}

func TestErrorsPropagateThroughOperands(t *testing.T) {
	if got := bad(t, nil, "@x = 1 + @nope").Reason; !strings.Contains(got, "not bound") {
		t.Errorf("got %q", got)
	}
}

func TestEqualityAcrossTypes(t *testing.T) {
	cases := []struct{ src, want string }{
		{"@x = 1 is true", "false"},
		{"@x = true is true", "true"},
		{"@x = [1] is [1, 2]", "false"},
		{"@x = { a: 1 } is { b: 1 }", "false"},
		{"@x = { a: 1 } is { a: 1 }", "true"},
		{`@x = "1" is 1`, "false"},
	}
	for _, c := range cases {
		v, _ := ok(t, nil, c.src)
		eq(t, v, c.want)
	}
}

// --- verb failure paths ------------------------------------------------------

func TestVerbsRejectNonTextLocations(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{"@x = fetch 5", "fetch needs a URL"},
		{"@x = read 5", "read needs a path"},
		{"@x = read", "read needs a path"},
		{"@x = fetch", "fetch needs a URL"},
		{"@d = 1\nwrite 5 @d", "write needs a path"},
		{`send "x" to 5`, "send needs a destination"},
	} {
		if got := bad(t, nil, c.src).Reason; !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.src, got, c.want)
		}
	}
}

func TestReadWithExplicitFormat(t *testing.T) {
	h := host.NewFake()
	h.Files["./data.txt"] = `{"a": 1}`
	// Without `as json` the extension decides, and .txt means text.
	v, _ := ok(t, h, "@x = read ./data.txt")
	eq(t, v, `{"a": 1}`)
	// An explicit `as` clause always wins over the extension.
	v, _ = ok(t, h, "@x = read ./data.txt as json")
	eq(t, v, "{ a: 1 }")
}

func TestReadFromClause(t *testing.T) {
	h := host.NewFake()
	h.Files["./input.csv"] = "a,b"
	v, _ := ok(t, h, "@x = read data from ./input.csv")
	eq(t, v, "a,b")
}

func TestWriteFailurePaths(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{"write ./out.json", "write needs something to write"},
		{"write ./out.json @nope", "not bound"},
		{"write @nope to ./out.json", "not bound"},
		{"@d = [1]\nwrite @d to ./out.csv as csv", "csv needs a list of records"},
		{`@d = "x"` + "\nwrite @d to ./out.csv as csv", "csv needs a list of records"},
	} {
		if got := bad(t, nil, c.src).Reason; !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.src, got, c.want)
		}
	}
}

func TestWriteCSVFillsMissingColumns(t *testing.T) {
	// A row missing a field must yield an empty cell, not a shifted table.
	_, h := ok(t, nil, `@rows = [{ a: 1, b: 2 }, { a: 3 }]
write @rows to ./out.csv as csv`)
	if got, want := h.Written[0].Content, "a,b\n1,2\n3,"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestWriteCSVQuotesCellsThatNeedIt(t *testing.T) {
	_, h := ok(t, nil, `@rows = [{ a: "x,y" }]
write @rows to ./out.csv as csv`)
	if !strings.Contains(h.Written[0].Content, `"x,y"`) {
		t.Errorf("got %q", h.Written[0].Content)
	}
}

func TestCreateFileFailurePaths(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{`create file with "x"`, "needs `at <path>`"},
		{`create file at ./x.md`, "needs `with <content>`"},
		{`create file at 5 with "x"`, "create file needs a path"},
		{`create file at ./x.md with @nope`, "not bound"},
	} {
		if got := bad(t, nil, c.src).Reason; !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.src, got, c.want)
		}
	}
}

func TestSendPipedValue(t *testing.T) {
	_, h := ok(t, nil, `[1, 2, 3] |> count |> send to output`)
	if got := strings.TrimSpace(h.Stdout.String()); got != "3" {
		t.Errorf("got %q", got)
	}
}

func TestSendToUserGoesToOutput(t *testing.T) {
	_, h := ok(t, nil, `send "hi" to user`)
	if !strings.Contains(h.Stdout.String(), "hi") {
		t.Errorf("got %q", h.Stdout.String())
	}
}

func TestSendPropagatesAFailedPayload(t *testing.T) {
	if got := bad(t, nil, "send @nope to output").Reason; !strings.Contains(got, "not bound") {
		t.Errorf("got %q", got)
	}
}

func TestSendToADeadURLReportsIt(t *testing.T) {
	if got := bad(t, nil, `send "x" to https://down.example.com/hook`).Reason; !strings.Contains(got, "connection refused") {
		t.Errorf("got %q", got)
	}
}

func TestAskFailurePaths(t *testing.T) {
	if got := bad(t, nil, "ask").Reason; !strings.Contains(got, "ask needs a prompt") {
		t.Errorf("got %q", got)
	}
	// No answers scripted: the host reports it rather than inventing one.
	if got := bad(t, nil, `@a = ask "well?"`).Reason; !strings.Contains(got, "no answer available") {
		t.Errorf("got %q", got)
	}
}

func TestAskAcceptsAPipedPrompt(t *testing.T) {
	h := host.NewFake()
	h.Answers = []string{"sure"}
	v, h := ok(t, h, `@a = "proceed?" |> ask`)
	eq(t, v, "sure")
	if h.Asked[0] != "proceed?" {
		t.Errorf("got %v", h.Asked)
	}
}

func TestRunToolFailurePaths(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{"@x = run tool", "run needs a shell command"},
		{"@x = run tool 5", "a tool name must be a word"},
		{"@x = run tool search with @nope", "not bound"},
	} {
		if got := bad(t, nil, c.src).Reason; !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.src, got, c.want)
		}
	}
}

func TestToolReturningAnErrorIsReported(t *testing.T) {
	h := host.NewFake()
	h.Register("flaky", func(host.Call) object.Value { return host.Fail("tool exploded") })
	if got := bad(t, h, "use flaky\n"+`@x = run tool flaky with q "a"`).Reason; !strings.Contains(got, "tool exploded") {
		t.Errorf("got %q", got)
	}
}

func TestWhereOnANonListIsAnError(t *testing.T) {
	h := host.NewFake()
	h.Responses["https://api.com/one"] = `{"a":1}`
	if got := bad(t, h, `@x = fetch https://api.com/one where a is 1`).Reason; !strings.Contains(got, "needs a list to filter") {
		t.Errorf("got %q", got)
	}
}

func TestWhereFailurePropagatesFromInsideTheFilter(t *testing.T) {
	h := host.NewFake()
	h.Responses["https://api.com/list"] = `[{"a":1}]`
	if got := bad(t, h, `@x = fetch https://api.com/list where nope is 1`).Reason; !strings.Contains(got, `no field "nope"`) {
		t.Errorf("got %q", got)
	}
}

// --- transform failure paths -------------------------------------------------

func TestTransformArgumentErrors(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{"@x = [1] |> filter", "filter needs a condition"},
		{"@x = [1] |> map", "map needs an expression"},
		{"@x = [1] |> group", "group needs a key"},
		{"@x = [1] |> take", "take needs a count"},
		// A negative count arrives bound: a bare `-1` is not an argument
		// starter, so `take -1` reports the missing count instead.
		{"@n = 0 - 1\n@x = [1] |> take @n", "non-negative number"},
		{"@x = [1] |> take -1", "take needs a count"},
		{`@x = [1] |> take "a"`, "non-negative number"},
		{"@x = [1] |> take @nope", "not bound"},
		{"@x = [1] |> matches", "matches needs a pattern"},
		{`@x = ["a"] |> sum`, "sum needs numbers"},
		{"@x = [1] |> trim", "trim needs strings"},
		{`@x = 5 |> trim`, "needs a string or a list of strings"},
		{"@x = 5 |> filter where a", "filter needs a list"},
	} {
		if got := bad(t, nil, c.src).Reason; !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.src, got, c.want)
		}
	}
}

func TestTakeBeyondTheEndReturnsEverything(t *testing.T) {
	v, _ := ok(t, nil, "@x = [1, 2] |> take 10")
	eq(t, v, "[1, 2]")
}

func TestCountOfARecordAndAString(t *testing.T) {
	v, _ := ok(t, nil, "@x = { a: 1, b: 2 } |> count")
	eq(t, v, "2")
	v, _ = ok(t, nil, `@x = "abc" |> count`)
	eq(t, v, "3")
}

func TestSortWithAFailingKey(t *testing.T) {
	if got := bad(t, nil, "@x = [{ a: 1 }] |> sort by nope").Reason; !strings.Contains(got, `no field "nope"`) {
		t.Errorf("got %q", got)
	}
}

func TestSelfOutsideATransformIsAnError(t *testing.T) {
	if got := bad(t, nil, "@x = @.name").Reason; !strings.Contains(got, "only meaningful inside a transform") {
		t.Errorf("got %q", got)
	}
}

func TestFieldAccessOnANonRecord(t *testing.T) {
	if got := bad(t, nil, `@x = "abc"`+"\n@y = @x.field").Reason; !strings.Contains(got, "cannot read field") {
		t.Errorf("got %q", got)
	}
}

func TestFilterOnAListOfScalarsNamesTheProblem(t *testing.T) {
	if got := bad(t, nil, `@x = [1, 2] |> filter where a`).Reason; !strings.Contains(got, "cannot read field") {
		t.Errorf("got %q", got)
	}
}

// TestContextNowAndToday: v2 §16 uses context.env.now for a timestamp while
// v1 §12 uses context.env.today for a date. They are different things, so both
// exist rather than one replacing the other.
func TestContextNowAndToday(t *testing.T) {
	v, _ := ok(t, nil, "@x = context.env.today")
	eq(t, v, "2026-08-07")
	v, _ = ok(t, nil, "@x = context.env.now")
	eq(t, v, "2026-08-07T09:00:00Z")
}
