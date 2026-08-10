// Command mana runs Mana scripts, or starts a REPL when given no script.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"

	"github.com/typedmirror/mana/internal/host"
	"github.com/typedmirror/mana/internal/repl"
	"github.com/typedmirror/mana/internal/serve"
)

// Version is the release string, tracking the language specification it
// implements rather than counting its own releases: an interpreter for spec
// v0.2 is more useful to know than that this is its third build.
//
// `make build` stamps it from the git tag; a plain `go build` reports the
// constant below with a -dev suffix, so an unstamped binary never claims to be
// a release.
var Version = "0.2.0-dev"

const usage = `mana — an interpreted intent script for LLM agents

usage:
  mana <file.mana>       execute a script
  mana                   start the REPL
  mana serve             sessions over HTTP, loopback by default

flags:
  --dry-run              report what the script would do, cause nothing
  --json                 emit the run report as JSON, for a machine reader
  --trace                print the execution record afterwards, on stderr
  --retry N              give a failed act N extra attempts
  --timeout D            bound each shell command (default 2m)
  --addr A               serve address (default 127.0.0.1:7777)
  --tokens               print the token stream, intent channel included
  --help                 show this message
  --version              print the version

serve:
  a session is a persistent context window: flat scripts share bindings
  across submissions, act scripts run as self-contained jobs. Responses are
  the --json report; ok:false is the failure signal, HTTP status codes are
  the transport's own. Set MANA_SERVE_TOKEN to require a bearer token.

exit codes:
  0  success
  1  runtime error (a verb failed, or a failure went unhandled)
  2  parse error
  3  file not found

Script output goes to stdout; everything the runtime says about the run goes to
stderr, so redirecting a script yields only what the script sent.
`

func main() { os.Exit(run(os.Args[1:])) }

// run returns the process exit code rather than calling os.Exit itself, so
// every path is reachable from a test.
func run(args []string) int {
	fs_ := flag.NewFlagSet("mana", flag.ContinueOnError)
	fs_.SetOutput(os.Stderr)
	// flag calls Usage itself before returning ErrHelp. Silencing it here means
	// `--help` prints once, on stdout, where a pipe can catch it.
	fs_.Usage = func() {}
	tokens := fs_.Bool("tokens", false, "print the token stream instead of running")
	version := fs_.Bool("version", false, "print the version")
	trace := fs_.Bool("trace", false, "print the execution record after the run")
	dryRun := fs_.Bool("dry-run", false, "report what the script would do, without doing it")
	retries := fs_.Int("retry", 0, "extra attempts for a failed act")
	asJSON := fs_.Bool("json", false, "emit the run report as JSON")
	timeout := fs_.Duration("timeout", 0, "bound on each shell command (default 2m)")
	addr := fs_.String("addr", "127.0.0.1:7777", "address for `mana serve`")
	if err := fs_.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(os.Stdout, usage)
			return repl.ExitOK
		}
		return repl.ExitParse
	}
	if *version {
		fmt.Fprintln(os.Stdout, "mana "+Version)
		return repl.ExitOK
	}

	h := host.NewReal(os.Stdout, os.Stderr, os.Stdin)
	// The binary decides what the environment provides (D-046). A script still
	// reaches a module only through `use`, per act.
	h.Register(host.NewClaude())
	for _, m := range host.MCPFromEnv() {
		h.Register(m)
	}
	rest := fs_.Args()
	if len(rest) > 0 && rest[0] == "serve" {
		// The flag package stops at the first positional, so `mana serve
		// --addr X` would silently drop the flag. Parse what followed the
		// subcommand too; both orders work.
		if err := fs_.Parse(rest[1:]); err != nil {
			return repl.ExitParse
		}
		return runServe(*addr, serve.Options{
			Token:   os.Getenv("MANA_SERVE_TOKEN"),
			Timeout: *timeout,
			Retries: *retries,
		})
	}
	if len(rest) == 0 {
		if *tokens {
			fmt.Fprintln(os.Stderr, "mana: --tokens needs a script")
			return repl.ExitParse
		}
		if err := repl.Start(os.Stdin, h); err != nil {
			fmt.Fprintf(os.Stderr, "mana: %v\n", err)
			return repl.ExitRuntime
		}
		return repl.ExitOK
	}

	src, err := os.ReadFile(rest[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "mana: %v\n", err)
		if errors.Is(err, fs.ErrNotExist) {
			return repl.ExitNoFile
		}
		return repl.ExitRuntime
	}
	if *tokens {
		repl.DumpTokens(string(src), os.Stdout)
		return repl.ExitOK
	}
	if *retries < 0 {
		fmt.Fprintln(os.Stderr, "mana: --retry cannot be negative")
		return repl.ExitParse
	}
	return repl.RunWith(string(src), h, repl.Options{
		Retries: *retries,
		Trace:   *trace,
		DryRun:  *dryRun,
		JSON:    *asJSON,
		Timeout: *timeout,
	})
}

// runServe starts the session server. Each session's host mirrors the
// binary's own wiring (D-046) — same modules, same defaults — with output
// captured per run and no interactive stdin: `ask` over the wire has no
// answer channel yet, and failing honestly beats hanging a request.
func runServe(addr string, opts serve.Options) int {
	newHost := func() host.Host {
		h := host.NewReal(os.Stdout, os.Stderr, strings.NewReader(""))
		h.Register(host.NewClaude())
		for _, m := range host.MCPFromEnv() {
			h.Register(m)
		}
		return h
	}
	s := serve.New(newHost, opts)
	guard := "loopback trust"
	if opts.Token != "" {
		guard = "bearer token required"
	}
	fmt.Fprintf(os.Stderr, "mana serve listening on %s (%s)\n", addr, guard)
	if err := http.ListenAndServe(addr, s.Handler()); err != nil {
		fmt.Fprintf(os.Stderr, "mana serve: %v\n", err)
		return repl.ExitRuntime
	}
	return repl.ExitOK
}
