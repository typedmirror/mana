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
  mana <file.mana>     execute a script
  mana                 start the REPL
  mana --tokens <file> print the token stream, intent channel included
  mana --help          show this message
  mana --version       print the version

exit codes:
  0  success
  1  runtime error (a verb failed, or a failure went unhandled)
  2  parse error
  3  file not found
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
	return repl.Run(string(src), h)
}
