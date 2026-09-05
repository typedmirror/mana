package host

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/typedmirror/mana/internal/object"
)

// stubMCP returns a bridge wired to the committed stub server. The path is
// resolved from the repo layout because the subprocess runs through $SHELL -c
// in this package's own directory.
func stubMCP(t *testing.T) *MCP {
	t.Helper()
	path, err := filepath.Abs("../../tests/mcp_stub.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stub missing: %v", err)
	}
	m := NewMCP("stub", path)
	t.Cleanup(func() { m.mu.Lock(); m.kill(); m.mu.Unlock() })
	return m
}

func withRecord(pairs ...string) map[string]object.Value {
	rec := object.NewRecord()
	for i := 0; i+1 < len(pairs); i += 2 {
		rec.Set(pairs[i], object.String(pairs[i+1]))
	}
	return map[string]object.Value{"with": rec}
}

// --- failure paths first -----------------------------------------------------

func TestMCPUnknownToolListsTheVocabulary(t *testing.T) {
	m := stubMCP(t)
	v := m.Execute(Call{Target: "no_such_tool"})
	bad := mustErr(t, v)
	if !strings.Contains(bad.Suggestion, "explode") || !strings.Contains(bad.Suggestion, "lookup") || !strings.Contains(bad.Suggestion, "reflect") {
		t.Errorf("suggestion must carry the vocabulary: %q", bad.Suggestion)
	}
}

func TestMCPDeclaredToolFailureIsAnError(t *testing.T) {
	m := stubMCP(t)
	bad := mustErr(t, m.Execute(Call{Target: "explode"}))
	if !strings.Contains(bad.Reason, "exploded, as designed") {
		t.Errorf("the server's own words must survive: %q", bad.Reason)
	}
}

func TestMCPMissingServerFailsWithTheEnvName(t *testing.T) {
	m := NewMCP("ghost", "/definitely/not/a/server")
	bad := mustErr(t, m.Execute(Call{Target: "anything"}))
	if !strings.Contains(bad.Suggestion, "MANA_MCP_GHOST") {
		t.Errorf("suggestion: %q", bad.Suggestion)
	}
}

func TestMCPHungServerIsKilledNotWaitedOn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hang.sh")
	os.WriteFile(path, []byte("#!/bin/sh\nsleep 60\n"), 0o755)
	m := NewMCP("hang", path)
	m.timeout = 100 * time.Millisecond
	start := time.Now()
	bad := mustErr(t, m.Execute(Call{Target: "anything"}))
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("waited %s on a hung server", elapsed)
	}
	if !strings.Contains(bad.Reason, "cannot start") {
		t.Errorf("reason: %q", bad.Reason)
	}
}

func TestMCPNeedsAToolName(t *testing.T) {
	m := stubMCP(t)
	bad := mustErr(t, m.Execute(Call{}))
	if !strings.Contains(bad.Reason, "tool name") {
		t.Errorf("reason: %q", bad.Reason)
	}
}

func TestMCPArgumentsMustBeARecord(t *testing.T) {
	m := stubMCP(t)
	bad := mustErr(t, m.Execute(Call{
		Target:  "lookup",
		Clauses: map[string]object.Value{"with": object.Number(7)},
	}))
	if !strings.Contains(bad.Suggestion, "with {") {
		t.Errorf("suggestion: %q", bad.Suggestion)
	}
}

// --- the working half --------------------------------------------------------

func TestMCPLookupAsJSON(t *testing.T) {
	m := stubMCP(t)
	v := m.Execute(Call{
		Target:  "lookup",
		Clauses: map[string]object.Value{"with": object.NewRecord(), "as": object.Word("json")},
	})
	rec, ok := v.(*object.Record)
	if !ok {
		t.Fatalf("got %s: %s", v.Type(), v.Inspect())
	}
	if region, _ := rec.Get("region"); region.Inspect() != "eu" {
		t.Errorf("region: %v", region)
	}
}

// The `--` line crosses the protocol boundary in _meta (D-053): the stub's
// reflect tool answers with whatever intent it was handed.
func TestMCPIntentCrossesTheWire(t *testing.T) {
	m := stubMCP(t)
	v := m.Execute(Call{Target: "reflect", Intent: "checking the bridge carries the reasoning"})
	s, ok := v.(object.String)
	if !ok {
		t.Fatalf("got %s: %s", v.Type(), v.Inspect())
	}
	if string(s) != "intent received: checking the bridge carries the reasoning" {
		t.Errorf("got %q", s)
	}
}

func TestMCPToolNameCanBeAStringArgument(t *testing.T) {
	m := stubMCP(t)
	v := m.Execute(Call{Args: []object.Value{object.String("lookup")}})
	if _, ok := v.(object.String); !ok {
		t.Fatalf("got %s: %s", v.Type(), v.Inspect())
	}
}

func TestMCPCallsSerializeSafely(t *testing.T) {
	m := stubMCP(t)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if v := m.Execute(Call{Target: "lookup"}); object.IsErr(v) {
				t.Errorf("unexpected failure: %s", v.Inspect())
			}
		}()
	}
	wg.Wait()
}

func TestMCPFromEnvScansAndNames(t *testing.T) {
	t.Setenv("MANA_MCP_WIKI", "some-command")
	t.Setenv("MANA_MCP_ISSUES", "another-command")
	var found []string
	for _, m := range MCPFromEnv() {
		found = append(found, m.Name())
	}
	joined := strings.Join(found, ",")
	if !strings.Contains(joined, "wiki") || !strings.Contains(joined, "issues") {
		t.Errorf("got %v", found)
	}
}

// An _EFFECTS declaration is a footprint, never a server (D-056), and the
// declared footprint reaches the module.
func TestMCPEffectsDeclarationIsNotAServer(t *testing.T) {
	t.Setenv("MANA_MCP_WIKI", "some-command")
	t.Setenv("MANA_MCP_WIKI_EFFECTS", "network, fs_read")
	mods := MCPFromEnv()
	if len(mods) != 1 || mods[0].Name() != "wiki" {
		t.Fatalf("the _EFFECTS key must not register a module: %v", mods)
	}
	eff := mods[0].Effects()
	if len(eff) != 2 || eff[0] != "network" || eff[1] != "fs_read" {
		t.Errorf("effects: %v", eff)
	}
}

// Undeclared stays nil — the bridge does not guess (D-056).
func TestMCPUndeclaredEffectsAreNil(t *testing.T) {
	t.Setenv("MANA_MCP_PLAIN", "some-command")
	mods := MCPFromEnv()
	for _, m := range mods {
		if m.Name() == "plain" && m.Effects() != nil {
			t.Errorf("undeclared must be nil, got %v", m.Effects())
		}
	}
}
