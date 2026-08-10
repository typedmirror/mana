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
mana --json job.mana          # one document: status, per-step outcomes, output
mana --dry-run job.mana       # what it would do, causes nothing
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

A host that binds nothing still answers honestly: a script asking for modules
gets an empty list rather than a plausible-looking one.

---

## Status

**Language v0.2 is implemented**, except the MCP bridge. 285 test cases,
race-clean, zero dependencies, ~10.3k lines.

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

- **MCP bridge** — one adapter over the module interface. Parked.
- **Cross-invocation resume** — retry and resume *within* a run work. Resuming a
  job in a later invocation needs a staleness model, and a cache with no defined
  invalidation is a promise the runtime cannot keep.
- **`mana serve`** — needs decisions nobody has made: what it exposes, how a job
  is addressed, what authenticates a caller.
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

`docs/specv2.md` is the language definition. `docs/spec.md` is v0.1, kept
because the v0.1 interpreter is still what most of the runtime is.

The specs are held to the code mechanically: every `mana` block in v0.1 is
parsed by the test suite, and its reference program must stay embedded verbatim
in two suites. Where the two disagreed, it was almost always the document that
moved — the implementation had been following the spec's own examples more
faithfully than its prose did.

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

