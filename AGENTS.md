# Mana for agents

You are about to emit `.mana`. This is the language as it actually behaves,
written for a cold agent, every form verified against the current binary.
When something is not in the language, this file says so instead of offering
a workaround. The failure value everywhere is one shape:
`{ status: err, at, intent, reason, suggestion }` — binding one is legal,
ignoring one fails the job at exit.

The one protocol that matters: put a `--` line above what you do. It is not
a comment. It rides into every failure, every subagent's system prompt,
every MCP call's `_meta`, and the report's step records. One line is the
whole discipline.

## Verbs

- `fetch <url>` · `fetch <field> from <url> [where <cond>]` — HTTP GET.
  JSON decodes to structure, anything else stays a string; `<field> from`
  selects a key from a record response; `where` filters a list result.
- `read <path> [as json|csv|text]` — file to string; `as json` parses hard;
  a `.json` extension parses by default.
- `run <raw line to end of line>` — the line belongs to the shell (or to
  argv, in a kernel). Continuations start the NEXT line: `or <fallback>`,
  `with { KEY: @value }` (environment, as data). Success is stdout with one
  trailing newline trimmed; stderr-on-success becomes a step note; nonzero
  exit fails with `exit N: <stderr>`. `run tool <name> with { … }` calls a
  module instead.
- `create file at <path> with <content>` — the only create provider.
- `write <data> to <path> [as json|csv]` — also `write <path> <data>`.
- `send <value>` — inside an act: sets the act's result, once.
  `send err <reason>` — inside an act: fails it.
  `send <value> to output|user|<url>|<used module> [clauses]` — emit
  outward. Emitting is not result-setting: a file that only sends to output
  produces no result, standalone or imported.
- `ask <prompt>` — the OUTER channel: whoever invoked the run. Not for
  delegation (modules are). `ask tools` lists modules. Fails honestly when
  stdin has no answer.

## Transforms (twelve; `|>` pipes, `->` binds tight)

`filter where <cond>` (per element, `@` is the element — inside a condition
use `->` or parentheses, because `|>` binds to the whole pipeline:
`filter where email -> matches ".+@.+"` ✓,
`filter where @ |> matches "x"` ✗ hands the list to matches) ·
`map <expr|field>` · `sort [by <key>] [descending]` (stable) ·
`group by <key>` → list of `{ key, count, items }` · `take <n>` · `count` ·
`sum` · `trim` / `lowercase` (elementwise on string lists) ·
`matches <regexp>` (string → bool, NOT elementwise) ·
`parse` (JSON string → value, hard error on prose, NOT elementwise) ·
`lines` (string → list of lines; a list of strings flattens).

## Bindings and flow

`@name = <expr>` — one sigil, rebindable. `or` unwraps: after
`@x = <fails> or <fb>`, `@x` IS the fallback and a later `match` takes the
`ok` arm — catch with `or` OR dispatch with `match`, never both on one
binding. `match { ok <b>: …  err <b>: … }` needs a piped subject.
`if <c> then <a> else <b>` is an expression.

Scope: an act sees its own bindings plus `act.<dep>.result` for DECLARED
dependencies only — reading an undeclared act's result is an error (it
would be a race). Never a sibling's `@` bindings, by design. A flat script
is one unnamed act; mixing loose statements with `act` declarations is
rejected everywhere (run, dry-run, emission).

`act "name" from ./file.mana` imports a FLAT file as the act's body. The
imported file must not declare acts (so cross-file cycles cannot exist).
Inside it, `use` is lifted onto the act, and result-form `send <value>`
sets the act result — `send … to output` only emits, exactly as it would
standalone.

## Modules

`use <name>` — per-act grant, the only permission boundary. The `with`
clause means three different things by context: on `run` it is environment
variables; on `claude` it is prompt context (`with { model: "opus" }` picks
the model, other keys become labelled blocks); on an MCP module it is the
tool's JSON arguments verbatim. `as json` on any module call parses the
reply hard. Your `--` line above the call arrives for real: claude gets it
as system-prompt context, MCP servers in `_meta["mana/intent"]`.

## Hosts

| | `run` realized as | same-line `$KEY` | the guard sees | with-env arrives |
|---|---|---|---|---|
| Real | `$SHELL -c`, env as exports ahead of the line | works | — | child env, expandable |
| Kernel (`MANA_KERNEL`) | in-kernel argv-direct | never (no shell) | real basename | merged into subprocess env |

Portable env forms: commands that READ the environment (`printenv K`), or
`sh -c '…$K…'` at the visible price that a guard sees only `sh`.

## Reading the report (`--json`)

Key on: `ok` · `failures[]` (each failure once: act, at, intent, reason,
suggestion) · `skipped[]` · `acts[].status` (`ok|failed|skipped|reused`) ·
`steps[].status` (`ok|failed|recovered|halted` — `recovered` means this
step's own failure was consumed by `or`; `halted` means an earlier failure
stopped it) · `steps[].effects` (mutations that fired — retried attempts
keep their steps, labelled `attempt N failed · …`, so effects sum to what
actually ran) vs `steps[].reads` (observed) · `acts[].attempts` (present
only when retried). `--retry N` re-runs failed acts (fresh evaluator,
succeeded deps keep results, module effects re-fire — there is no
idempotence protection). `--timeout D` bounds each shell command and each
module/kernel call. `--dry-run` is the plan for humans; `--emit-envelope`
is the per-act capability family for machines; both cause nothing.

Serve: session-scoped = flat-script bindings and the last act-job's results
(identity-based reuse; effects not re-fired; `?fresh=1` bypasses).
Job-scoped = everything inside one act script. Act results never become
session bindings; the report is the carry channel.

## Not in the language

Loops. User-defined functions. String escape sequences (multiline literals
are the only out). String interpolation anywhere — environment is the only
crossing into `run`. `split` by arbitrary separator (`lines` only). Postfix
`as json` on a binding (`parse` is the transform). Mixing flat statements
with acts. Sibling binding access. Module definitions in-language.
`test`/`mock`/`assert` vocabulary. Cross-run `act.x.result` in serve.
Deterministic ordering of concurrent `send to output` (treat interleave as
nondeterministic).
