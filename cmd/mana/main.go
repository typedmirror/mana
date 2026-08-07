// Command mana runs Mana scripts, or starts a REPL when given no script.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"

	"github.com/typedmirror/mana/internal/host"
	"github.com/typedmirror/mana/internal/repl"
)

// Version is the release string. Set at build time with
// -ldflags "-X main.Version=…"; the default is what a `go build` reports.
var Version = "0.1.0-dev"

const usage = `mana — an interpreted intent script for LLM agents

usage:
  mana <file.mana>       execute a script
  mana                   start the REPL

flags:
  --dry-run              report what the script would do, cause nothing
  --trace                print the execution record afterwards, on stderr
  --retry N              give a failed act N extra attempts
  --tokens               print the token stream, intent channel included
  --help                 show this message
  --version              print the version

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
	rest := fs_.Args()
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
	})
}
