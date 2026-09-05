package serve

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/typedmirror/mana/internal/host"
	"github.com/typedmirror/mana/internal/object"
)

func testServer(t *testing.T, opts Options) *httptest.Server {
	t.Helper()
	s := New(func() host.Host {
		h := host.NewFake()
		h.Shells["echo hi"] = host.Shell{Stdout: "hi\n"}
		h.Register("oracle", func(c host.Call) object.Value {
			return object.String("the answer")
		})
		return h
	}, opts)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func post(t *testing.T, url, body string, hdr map[string]string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var doc map[string]any
	if resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
			t.Fatalf("response is not JSON: %v", err)
		}
	}
	return resp.StatusCode, doc
}

func mkSession(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	code, doc := post(t, ts.URL+"/sessions", "", nil)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, doc)
	}
	return doc["session"].(string)
}

// --- failure paths first -----------------------------------------------------

// A runtime failure is a well-formed answer, not a transport fault (D-050).
func TestRuntimeFailureIs200WithOKFalse(t *testing.T) {
	ts := testServer(t, Options{})
	code, doc := post(t, ts.URL+"/run", "-- reaching for the absent\n@a = read ./absent.json\nsend @a to output", nil)
	if code != http.StatusOK {
		t.Fatalf("status %d, want 200", code)
	}
	if doc["ok"] != false {
		t.Errorf("ok: %v", doc["ok"])
	}
	failures := doc["failures"].([]any)
	first := failures[0].(map[string]any)
	if first["intent"] != "reaching for the absent" {
		t.Errorf("intent must cross the wire: %v", first)
	}
}

func TestParseErrorIs422(t *testing.T) {
	ts := testServer(t, Options{})
	code, doc := post(t, ts.URL+"/run", "@x = = nonsense", nil)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %v", code, doc)
	}
	if _, has := doc["parse_errors"]; !has {
		t.Errorf("body must list the parse errors: %v", doc)
	}
}

func TestUnknownSessionIs404(t *testing.T) {
	ts := testServer(t, Options{})
	code, _ := post(t, ts.URL+"/sessions/nope/run", "send 1 to output", nil)
	if code != http.StatusNotFound {
		t.Errorf("status %d, want 404", code)
	}
}

func TestEmptyScriptIs400(t *testing.T) {
	ts := testServer(t, Options{})
	code, _ := post(t, ts.URL+"/run", "   \n", nil)
	if code != http.StatusBadRequest {
		t.Errorf("status %d, want 400", code)
	}
}

// Hermes F4: the method-pattern mux answered 405 before auth ran, so an
// unauthenticated caller could enumerate which routes exist. With a token
// set, EVERY response — wrong methods and unknown paths included — requires
// the bearer first.
func TestWrongMethodStillNeedsTheToken(t *testing.T) {
	ts := testServer(t, Options{Token: "s3cret"})
	for _, probe := range []struct{ method, path string }{
		{"GET", "/run"},
		{"PUT", "/sessions"},
		{"OPTIONS", "/run"},
		{"GET", "/definitely-not-a-route"},
	} {
		req, _ := http.NewRequest(probe.method, ts.URL+probe.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s: %d, want 401 — nothing answers before auth", probe.method, probe.path, resp.StatusCode)
		}
	}
	// With the right token, the mux's own answers come through.
	req, _ := http.NewRequest("GET", ts.URL+"/run", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("authorized wrong method: %d, want 405", resp.StatusCode)
	}
}

func TestTokenGuardsEveryRoute(t *testing.T) {
	ts := testServer(t, Options{Token: "s3cret"})
	code, _ := post(t, ts.URL+"/run", "send 1 to output", nil)
	if code != http.StatusUnauthorized {
		t.Errorf("no token: %d, want 401", code)
	}
	code, _ = post(t, ts.URL+"/run", "send 1 to output", map[string]string{"Authorization": "Bearer wrong"})
	if code != http.StatusUnauthorized {
		t.Errorf("wrong token: %d, want 401", code)
	}
	code, _ = post(t, ts.URL+"/run", "send 1 to output", map[string]string{"Authorization": "Bearer s3cret"})
	if code != http.StatusOK {
		t.Errorf("right token: %d, want 200", code)
	}
}

func TestSessionCapFailsHonestly(t *testing.T) {
	ts := testServer(t, Options{})
	for i := 0; i < MaxSessions; i++ {
		if code, doc := post(t, ts.URL+"/sessions", "", nil); code != http.StatusCreated {
			t.Fatalf("create %d: %d %v", i, code, doc)
		}
	}
	code, doc := post(t, ts.URL+"/sessions", "", nil)
	if code != http.StatusConflict {
		t.Errorf("at cap: %d, want 409: %v", code, doc)
	}
}

// A failure reported in one response must not resurface in the next run's
// unhandled sweep — the REPL's reported-once rule (D-049).
func TestSessionFailureIsReportedOnce(t *testing.T) {
	ts := testServer(t, Options{})
	id := mkSession(t, ts)
	code, doc := post(t, ts.URL+"/sessions/"+id+"/run", "@bad = read ./absent.json", nil)
	if code != http.StatusOK || doc["ok"] != false {
		t.Fatalf("first run: %d %v", code, doc)
	}
	code, doc = post(t, ts.URL+"/sessions/"+id+"/run", "send 2 to output", nil)
	if code != http.StatusOK || doc["ok"] != true {
		t.Errorf("the old failure leaked into a later run: %v", doc)
	}
}

// --- the working half --------------------------------------------------------

func TestOneShotRunReturnsTheReport(t *testing.T) {
	ts := testServer(t, Options{})
	code, doc := post(t, ts.URL+"/run", "-- greeting\n@a = run echo hi\nsend @a to output", nil)
	if code != http.StatusOK || doc["ok"] != true {
		t.Fatalf("%d %v", code, doc)
	}
	if strings.TrimSpace(doc["output"].(string)) != "hi" {
		t.Errorf("output: %q", doc["output"])
	}
}

// The session is the context window: a binding made in one run is alive in
// the next (D-049).
func TestSessionBindingsPersistAcrossRuns(t *testing.T) {
	ts := testServer(t, Options{})
	id := mkSession(t, ts)
	if code, doc := post(t, ts.URL+"/sessions/"+id+"/run", `@name = "mana"`, nil); code != http.StatusOK || doc["ok"] != true {
		t.Fatalf("first run: %d %v", code, doc)
	}
	code, doc := post(t, ts.URL+"/sessions/"+id+"/run", `send "hello " + @name to output`, nil)
	if code != http.StatusOK || doc["ok"] != true {
		t.Fatalf("second run: %d %v", code, doc)
	}
	if strings.TrimSpace(doc["output"].(string)) != "hello mana" {
		t.Errorf("the binding did not survive: %q", doc["output"])
	}
}

// Each run's report describes that run alone, even on a shared evaluator.
func TestSessionReportsAreScopedPerRun(t *testing.T) {
	ts := testServer(t, Options{})
	id := mkSession(t, ts)
	post(t, ts.URL+"/sessions/"+id+"/run", "-- first artifact\n@a = 1", nil)
	_, doc := post(t, ts.URL+"/sessions/"+id+"/run", "-- second artifact\nsend @a to output", nil)
	acts := doc["acts"].([]any)
	steps := acts[0].(map[string]any)["steps"].([]any)
	if len(steps) != 1 {
		t.Fatalf("run 2 must carry only its own steps: %v", steps)
	}
	if steps[0].(map[string]any)["intent"] != "second artifact" {
		t.Errorf("got %v", steps[0])
	}
}

// Act scripts run as self-contained jobs inside a session; results return in
// the report, which is the carry channel (D-049).
func TestActJobInsideASession(t *testing.T) {
	ts := testServer(t, Options{})
	id := mkSession(t, ts)
	src := `act "a" {
    send 40
}

act "b" depends on "a" {
    send act.a.result + 2 to output
}`
	code, doc := post(t, ts.URL+"/sessions/"+id+"/run", src, nil)
	if code != http.StatusOK || doc["ok"] != true {
		t.Fatalf("%d %v", code, doc)
	}
	if strings.TrimSpace(doc["output"].(string)) != "42" {
		t.Errorf("output: %q", doc["output"])
	}
}

func TestSessionStateAndDelete(t *testing.T) {
	ts := testServer(t, Options{})
	id := mkSession(t, ts)
	post(t, ts.URL+"/sessions/"+id+"/run", `@x = 1`, nil)

	resp, err := http.Get(ts.URL + "/sessions/" + id)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	json.NewDecoder(resp.Body).Decode(&doc)
	resp.Body.Close()
	if b := fmt.Sprint(doc["bindings"]); !strings.Contains(b, "x") {
		t.Errorf("bindings: %v", doc)
	}

	req, _ := http.NewRequest("DELETE", ts.URL+"/sessions/"+id, nil)
	del, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Errorf("delete: %d", del.StatusCode)
	}
	code, _ := post(t, ts.URL+"/sessions/"+id+"/run", "send 1 to output", nil)
	if code != http.StatusNotFound {
		t.Errorf("after delete: %d, want 404", code)
	}
}

// Modules reach a session the same way they reach the binary: through the
// host, gated by use.
func TestSessionReachesAModule(t *testing.T) {
	ts := testServer(t, Options{})
	id := mkSession(t, ts)
	code, doc := post(t, ts.URL+"/sessions/"+id+"/run", "use oracle\n@a = oracle \"anything\"\nsend @a to output", nil)
	if code != http.StatusOK || doc["ok"] != true {
		t.Fatalf("%d %v", code, doc)
	}
	if strings.TrimSpace(doc["output"].(string)) != "the answer" {
		t.Errorf("output: %q", doc["output"])
	}
}

// The turn-saver (D-054): a job fails partway, the caller resubmits the
// corrected artifact, and the acts that already succeeded are reused — their
// effects fire exactly once across both submissions.
func TestSessionResubmissionReusesDoneWork(t *testing.T) {
	var fakes []*host.Fake
	s := New(func() host.Host {
		h := host.NewFake()
		h.Shells["provision"] = host.Shell{Stdout: "provisioned\n"}
		h.Shells["deploy-fixed"] = host.Shell{Stdout: "deployed\n"}
		fakes = append(fakes, h)
		return h
	}, Options{})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	id := mkSession(t, ts)
	broken := `act "setup" {
    @a = run provision
    send @a
}

act "ship" depends on "setup" {
    @b = run deploy-broken
    send @b
}`
	code, doc := post(t, ts.URL+"/sessions/"+id+"/run", broken, nil)
	if code != http.StatusOK || doc["ok"] != false {
		t.Fatalf("first run should fail: %d %v", code, doc)
	}

	fixed := strings.Replace(broken, "deploy-broken", "deploy-fixed", 1)
	code, doc = post(t, ts.URL+"/sessions/"+id+"/run", fixed, nil)
	if code != http.StatusOK || doc["ok"] != true {
		t.Fatalf("second run: %d %v", code, doc)
	}
	statuses := map[string]string{}
	for _, a := range doc["acts"].([]any) {
		rec := a.(map[string]any)
		statuses[rec["name"].(string)] = rec["status"].(string)
	}
	if statuses["setup"] != "reused" || statuses["ship"] != "ok" {
		t.Errorf("want setup reused and ship ok, got %v", statuses)
	}

	// The proof: provision ran once across both submissions.
	h := fakes[len(fakes)-1]
	count := 0
	for _, r := range h.Ran {
		if r.Command == "provision" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("provision fired %d times across two submissions, want 1", count)
	}

	// ?fresh=1 bypasses reuse: everything runs again.
	code, doc = post(t, ts.URL+"/sessions/"+id+"/run?fresh=1", fixed, nil)
	if code != http.StatusOK || doc["ok"] != true {
		t.Fatalf("fresh run: %d %v", code, doc)
	}
	for _, a := range doc["acts"].([]any) {
		rec := a.(map[string]any)
		if rec["status"] != "ok" {
			t.Errorf("fresh must re-run everything: %v", rec)
		}
	}
}

// Different sessions run concurrently without sharing state; the race
// detector is the assertion.
func TestSessionsAreIndependentAndConcurrent(t *testing.T) {
	ts := testServer(t, Options{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := mkSession(t, ts)
			bind := fmt.Sprintf("@mine = %d", i)
			if code, doc := post(t, ts.URL+"/sessions/"+id+"/run", bind, nil); code != http.StatusOK {
				t.Errorf("bind: %d %v", code, doc)
				return
			}
			_, doc := post(t, ts.URL+"/sessions/"+id+"/run", "send @mine to output", nil)
			if strings.TrimSpace(doc["output"].(string)) != fmt.Sprint(i) {
				t.Errorf("session %d leaked: %v", i, doc["output"])
			}
		}(i)
	}
	wg.Wait()
}
