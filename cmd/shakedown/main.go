// Command shakedown checks a folder of static files against the five
// listing conditions and prints pass or fail for each.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/clearedstack/shakedown/internal/check"
)

const usage = `usage:
  shakedown check [-json] [-timeout 30s] [-case-timeout 2s] <folder>
  shakedown version

exit status: 0 pass, 1 fail, 2 bad usage or unreadable folder,
             3 needs_subtool, 4 timeout
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "%s %s (contract %s)\n", check.Component, check.Version, check.Contract)
		return 0
	case "check":
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}

	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "print the report as JSON")
	timeout := fs.Duration("timeout", 30*time.Second, "deadline for the whole check")
	caseTimeout := fs.Duration("case-timeout", 2*time.Second, "deadline for each worked example")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 1 || *timeout <= 0 || *caseTimeout <= 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	report, err := check.Dir(ctx, fs.Arg(0), check.Options{CaseTimeout: *caseTimeout})
	if err != nil {
		fmt.Fprintf(stderr, "shakedown: %s: %v\n", check.EParse, err)
		return 2
	}
	write := report.WriteText
	if *asJSON {
		write = report.WriteJSON
	}
	if err := write(stdout); err != nil {
		return 2
	}
	switch report.Verdict {
	case check.Pass:
		return 0
	case check.NeedsSubtool:
		return 3
	case check.Timeout:
		return 4
	default:
		return 1
	}
}
