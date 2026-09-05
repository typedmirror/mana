# Mana

**An interpreted intent script for LLM agents.** Reasoning and execution in one
stream.

```mana
-- checking whether the service is reachable before deploying
@health = run curl -sf --max-time 2 localhost:3000/health

@health |> match {
    ok _:    -- it answered, ship it
             send "deploying" to output
    err why: send "not deploying: " + why to output
}
```

---

## Why this exists

Models already write this:

```
-- checking if the service is up
[tool call: bash("curl -sf localhost:3000/health")]
-- it's up, deploying
[tool call: bash("./deploy.sh")]
```

Two round trips. Each one is a full context re-read, an inference pass, a
dispatch, and a result injection. And the `--` line — the model's own statement
of what it was trying to do — is thrown away the moment the tool returns.

Mana makes that one artifact and one round trip, and keeps the reasoning. The
`--` lines are not comments: they are runtime metadata. When something fails,
the failure arrives carrying the intent that preceded it, without anyone writing
error-handling code.

```
$ mana deploy.mana
{ status: err
  at: "run curl -sf localhost:3000/health"
  intent: "checking whether the service is reachable before deploying"
  reason: "exit 7"
  suggestion: "raise the limit with --timeout, or background it: …" }
exit=1
```

That is the whole thesis. Everything else is in service of it.

---

## Install

Go 1.26+, no dependencies.

```sh
git clone <this repo> && cd mana
make build          # ./bin/mana
```

```sh
mana job.mana                 # run
mana                          # REPL
mana serve                    # sessions over HTTP, loopback by default
mana --json job.mana          # one document: status, per-step outcomes, output
mana --dry-run job.mana       # what it would do, causes nothing
mana --emit-envelope job.mana # per-act capability envelopes, causes nothing
mana --trace job.mana         # execution record, on stderr
mana --retry 2 job.mana       # give a failed act two more attempts
mana --timeout 30s job.mana   # bound each shell command
```

Exit codes: `0` success, `1` runtime failure, `2` parse error, `3` file not
found. Script output goes to stdout, everything the runtime says about the run
goes to stderr — so redirecting a job yields only what the job sent.

---

## The language in one page

**Seven verbs**, one per I/O boundary: `fetch` `read` `run` `create` `write`
`send` `ask`. **One binding sigil**, `@`. **Ten transforms** composed with `|>`
and `->`: `filter` `map` `sort` `group` `take` `count` `sum` `trim` `lowercase`
`matches`. **Clauses read by keyword, never position**: `where` `with` `from`
`to` `as` `at` `by`.

```mana
-- pull the day's numbers, falling back to yesterday's cache
@users = read ./cache/users.json
      or [
          { name: "alice", region: "eu", status: "active" }
          { name: "bob", region: "us", status: "inactive" }
          { name: "cara", region: "eu", status: "active" }
      ]

-- keep the ones that matter
@active = @users |> filter where status is "active"

-- summarise
@regions = @active |> group by region |> sort by count descending |> map key
send { total: @users -> count, active: @active -> count, regions: @regions } to output
```

**Errors are data.** There is no exception type and no panic path. A failed verb
returns a value that `or` can catch and `match` can dispatch on. Binding a
failure is legal — ignoring one is not: a failure nobody took responsibility for
is reported when the script exits.

**`run` is the shell, not a wrapper.** No escaping, no argument splitting.
Backgrounding is the shell's, because the shell already has it:

```
@server = run ./server > /dev/null 2>&1 &
```

---

## Acts

For work with structure, an **act** is a self-contained unit with its own
bindings, its own reasoning, its own permissions, and its own result. Acts
declare what they wait for; everything else runs at the same time.

```mana
act "collect" {
    send [
        { region: "eu", host: "a", hits: 200 }
        { region: "us", host: "b", hits: 5 }
        { region: "eu", host: "c", hits: 140 }
    ]
}

act "by-region" depends on "collect" {
    @e = act.collect.result
    @r = @e |> group by region |> map key
    send @r
}

act "top-hosts" depends on "collect" {
    @e = act.collect.result
    @h = @e |> filter where hits >= 100 |> map host
    send @h
}

act "report" depends on "by-region", "top-hosts" {
    send { regions: act.by-region.result, hosts: act.top-hosts.result } to output
}
```

`by-region` and `top-hosts` run concurrently. There are no async keywords
because the dependency graph already says everything there is to say.

A failed act stops its dependents, and they are reported as **skipped** — not as
successes. An act that never ran is not a pass.

```
$ mana --trace pipeline.mana
Trace: 4 act(s) in 41ms
  ├─ collect                [0ms → 38ms]       ok
  │      · 38ms    pull the day's events
  ├─ by-region              [38ms → 40ms]      ok
  │      deps:   [collect]
  ├─ top-hosts              [38ms → 40ms]      ok
  │      deps:   [collect]
  └─ report                 [40ms → 41ms]      ok
         deps:   [by-region, top-hosts]
```

A script with no `act` declarations is one unnamed act whose body is the file.
Nothing is special-cased.

---

## Modules

`use` loads a module and makes its name a verb. The `use` boundary is the
permission boundary — an act that did not ask for a module cannot reach it, even
when another act in the same job did.

```
act "notify" depends on "check" {
    use slack
    send "stock is low" to slack channel "ops"
}
```

Modules are Go packages implementing one interface. `Clauses()` may return nil,
which means the module accepts only the built-in keywords — that optionality is
what lets a plain tool and a clause-declaring module share one registry.

An undeclared clause fails with the vocabulary listed, and the reasoning
attached:

```
{ status: err
  at: "send \"deployed\" to slack thread \"42\""
  intent: "notify ops channel of deployment"
  reason: "module \"slack\" does not accept clause \"thread\""
  suggestion: "valid clauses for slack: [where, with, from, to, as, at, by, channel]" }
```

The binary binds one module: `claude`. An act that uses it delegates to a
model — prompt in, answer out, through the claude CLI. The caller's `--` line
rides into the subagent as system-prompt context, so a delegated model knows
*why* it was consulted. The subagent gets no tools: its answer is data, never
effects.

```
act "correctness" {
    use claude
    -- does the change hold together logically?
    @v = claude "review this diff for correctness" as json
    send @v
}
```

Three such acts with no edges between them are three concurrent subagents, and
`depends on` joins them — the act graph was already an orchestrator; now the
workers can be models. `examples/panel.mana` is the whole shape in thirty
lines. `MANA_CLAUDE_CMD` points the module at a stub executable for a
deterministic run, which is how the test suite exercises the full pathway
without ever invoking the live CLI. `as json` parses the reply with the same
machinery `read` uses — a subagent that answers prose where JSON was asked for
is a hard error, not a string that dies three stages later.

**Any MCP server is a module.** `MANA_MCP_<NAME>=<command>` bridges a server
in: it starts lazily on first use, speaks JSON-RPC on stdio, and its tools
become targets — `github search_issues with { query: "…" } as json`. The
caller's `--` line travels in the request's `_meta`, so the intent channel
crosses the protocol boundary too. A hung server is killed at the deadline
and restarts on the next call, and an unknown tool fails listing the server's
actual vocabulary.

A host that binds nothing still answers honestly: a script asking for modules
gets an empty list rather than a plausible-looking one.

---

## Capability envelopes

`--emit-envelope` derives, without executing anything, what each act may
touch: a per-act family of `{subprocess, network, fs_read, fs_write}`
coordinates plus an effect level (`pure` | `observe` | `io`), read off the
syntax tree the way `--dry-run` reads it. An enforcement substrate consumes
the family and grants each act exactly its coordinate — never the job-level
union, because an act that asked for nothing should hold nothing.

The projection is honest about its limits: a raw `run` line is `"*"` on
subprocess (the shell is a trust decision here, not a typed surface), a
computed path is `"*"`, and every contributor that forced a `"*"` is named
in a `top` list. Modules contribute their declared footprint —
`Effects() []string`, or `MANA_MCP_<NAME>_EFFECTS=network,fs_read` for a
bridged MCP server — and an undeclared module widens every axis rather than
guessing.

The other half of the contract is recovery: when an enforcement guard
denies an effect, the refusal comes back as an ordinary mana error — intent
attached, provenance ref preserved, catchable — so `or` can degrade the job
under an attenuated grant instead of dying:

```
-- live if granted, cache if not
@prices = run curl api.internal/prices
       or read ./cache/prices.json as json
```

Reports also witness the observed half: each step's `reads` lists the paths
and URLs it looked at, apart from `effects` (what it changed), because
confidentiality and integrity are different questions.

---

## Serve

`mana serve` is the REPL's session over the wire. A session is a persistent
context window: flat scripts share bindings across submissions, so a model can
run one artifact, read the report, and fire the next against what the first
one bound. Scripts with acts run as self-contained jobs; their results come
back in the report, which is the carry channel.

```
POST /sessions              → { "session": "…" }
POST /sessions/{id}/run     ← script body   → the report
GET  /sessions/{id}         → { "bindings": [...], "runs": N }
DELETE /sessions/{id}
POST /run                   one-shot, no session
```

The HTTP layer is not the error channel: a runtime failure returns 200 with
`ok:false` — the failure is a well-formed answer carrying intent and
suggestion, exactly as exit 1 is at the CLI. Status codes are the transport's
own: 422 parse errors, 404 unknown session, 401 bad token. Loopback by
default; set `MANA_SERVE_TOKEN` to require a bearer token.

A failed job does not cost the turn. The session keeps the last job's
successful act results, and a resubmitted artifact reuses them: an act whose
text is unchanged, with unchanged ancestors, reports `"reused"` — result
restored, body not run, **effects not fired again**. The dependency graph is
the staleness model: anything changed re-runs, and everything downstream of a
change re-runs with it. Fix the one broken act, resubmit the whole artifact,
and only the fix executes. `?fresh=1` bypasses reuse when the world moved
underneath an unchanged script. In memory only — the session's lifetime is
the cache's lifetime.

The report itself is designed for its actual reader — a model deciding its
next move: `ok`, then `output`, then `failures` (each one once, with the
intent that preceded it and a suggestion), then `skipped`, then per-act steps.
Steps carry their `effects` — the calls that changed something outside the
process — because the first question after a partial failure is *what did it
already do*. A step that a propagated failure stopped reads `halted`, not
`ok`: a step that never accomplished its intent is not a pass.

---

## Status

**Language v0.2 is fully implemented.** 331 test cases, race-clean, zero
dependencies, ~12.1k lines.

| package | | package | |
|---|---|---|---|
| token | 100% | parser | 90.5% |
| lexer | 95.9% | act | 89.9% |
| repl | 95.5% | object | 79.4% |
| evaluator | 91.7% | host | 79.1% |

`host`'s uncovered remainder is the real network, shell, and filesystem — the
part that cannot be unit-tested is exactly the part that isn't. A fake host
sits beside it, and `tests/` runs scripts against the real one so the two
cannot drift.

**Not built, on purpose:**

- **Cross-invocation resume** — retry and resume *within* a run work. Resuming a
  job in a later invocation needs a staleness model, and a cache with no defined
  invalidation is a promise the runtime cannot keep. Serve sessions sidestep
  this deliberately: session state is in-memory and dies with the process,
  which is a lifetime, not a cache.
- **`mana test`** — `test`/`mock`/`assert` are deliberately not language
  syntax, so there is nothing for it to run. Acts are tested from Go against a
  fake host.

**Known sharp edges**, all measured rather than suspected:

- `run` captures its whole line, so a fallback must start the next line:
  `run x` / `or read ./y.json`.
- `send @x |> f to output` is a syntax error, not what it looks like. Write
  `@x |> f |> send to output`.
- Module verbs resolve at statement or binding position: `@s = svc ping`, not
  `send svc ping`.
- `a/b` unspaced lexes as a path. Write `a / b`.
- Strings have no escape sequences.

---

## Specification

The language specification is maintained alongside the project but not in
this repository. It is held to the code mechanically: when present at
`docs/spec.md`, every ```mana block in it is parsed by the test suite (the
test skips on a clean clone), and the spec's reference program stays embedded
verbatim in two suites either way. Where spec and implementation disagreed,
it was almost always the document that moved — the implementation had been
following the spec's own examples more faithfully than its prose did.

---

## License

Licensed under either of

- Apache License, Version 2.0 ([LICENSE-APACHE](LICENSE-APACHE) or
  <http://www.apache.org/licenses/LICENSE-2.0>)
- MIT license ([LICENSE-MIT](LICENSE-MIT) or
  <http://opensource.org/licenses/MIT>)

at your option.

Unless you explicitly state otherwise, any contribution intentionally submitted
for inclusion in this work by you, as defined in the Apache-2.0 license, shall
be dual licensed as above, without any additional terms or conditions.

---

