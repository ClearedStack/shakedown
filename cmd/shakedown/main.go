// Command shakedown checks static tool bundles against the five listing
// conditions, either locally over a folder or in cluster worker mode against
// Muster.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ClearedStack/shakedown/internal/check"
	"github.com/ClearedStack/shakedown/internal/muster"
	"github.com/ClearedStack/shakedown/internal/spool"
	"github.com/ClearedStack/shakedown/internal/worker"
)

const (
	defaultMusterURL = "http://muster.clearedstack.svc.cluster.local:8080"
	defaultTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	defaultSpoolDir  = "/var/spool/cs"
)

const usage = `usage:
  shakedown check [-json] [-timeout 30s] [-case-timeout 2s] <folder>
  shakedown run [-once] [-poll 2s] [-heartbeat 20s] [-timeout 30s] [-case-timeout 2s]
  shakedown version

exit status: 0 pass, 1 fail, 2 bad usage or unreadable folder,
             3 needs_subtool, 4 timeout
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(runContext(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	return runContext(context.Background(), args, stdout, stderr)
}

func runContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "%s %s (contract %s)\n", check.Component, check.Version, check.Contract)
		return 0
	case "check":
		return runCheck(ctx, args[1:], stdout, stderr)
	case "run":
		return runWorker(ctx, args[1:], stderr)
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}
}

func runCheck(parent context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "print the report as JSON")
	timeout := fs.Duration("timeout", 30*time.Second, "deadline for the whole check")
	caseTimeout := fs.Duration("case-timeout", 2*time.Second, "deadline for each worked example")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || *timeout <= 0 || *caseTimeout <= 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}

	ctx, cancel := context.WithTimeout(parent, *timeout)
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

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func runWorker(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	once := fs.Bool("once", false, "drain spool and run at most one lease step")
	poll := fs.Duration("poll", worker.DefaultPollInterval, "idle poll interval")
	heartbeat := fs.Duration("heartbeat", worker.DefaultHeartbeatInterval, "lease heartbeat interval")
	timeout := fs.Duration("timeout", worker.DefaultCheckTimeout, "deadline for one bundle check")
	caseTimeout := fs.Duration("case-timeout", worker.DefaultCaseTimeout, "deadline for each worked example")
	musterURL := fs.String("muster-url", envOr("MUSTER_URL", defaultMusterURL), "Muster base URL")
	tokenFile := fs.String("token-file", envOr("SHAKEDOWN_TOKEN_FILE", defaultTokenFile), "ServiceAccount bearer token file")
	spoolDir := fs.String("spool-dir", envOr("SHAKEDOWN_SPOOL_DIR", defaultSpoolDir), "writable spool directory")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *poll <= 0 || *heartbeat <= 0 || *timeout <= 0 || *caseTimeout <= 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}

	contractVer := envOr("CONTRACT_VERSION", muster.Version)
	if contractVer != muster.Version {
		fmt.Fprintf(stderr, "shakedown: %s: unsupported contract version\n", muster.CodeNeedsSubtool)
		return 3
	}

	sp, err := spool.Open(*spoolDir)
	if err != nil {
		fmt.Fprintf(stderr, "shakedown: %v\n", err)
		return 2
	}

	cli := muster.NewClient(*musterURL, muster.FileTokenSource(*tokenFile))
	w, err := worker.New(worker.Config{
		Dispatcher:        cli,
		Spool:             sp,
		PollInterval:      *poll,
		HeartbeatInterval: *heartbeat,
		CheckTimeout:      *timeout,
		CaseTimeout:       *caseTimeout,
		Log:               stderr,
	})
	if err != nil {
		fmt.Fprintf(stderr, "shakedown: %v\n", err)
		return 2
	}

	if *once {
		if _, err := w.Step(ctx); err != nil {
			if muster.IsRetryable(err) {
				fmt.Fprintf(stderr, "shakedown: %s: transient dispatcher error\n", muster.CodeTimeout)
				return 4
			}
			fmt.Fprintf(stderr, "shakedown: %s: step failed\n", muster.CodeParse)
			return 1
		}
		return 0
	}

	if err := w.Run(ctx); err != nil {
		fmt.Fprintf(stderr, "shakedown: %s: worker stopped\n", muster.CodeParse)
		return 1
	}
	return 0
}
