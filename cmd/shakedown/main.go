// Command shakedown checks static tool bundles against the five listing
// conditions, either locally over a folder or in cluster worker mode against
// Muster.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/ClearedStack/shakedown/internal/check"
	"github.com/ClearedStack/shakedown/internal/muster"
	"github.com/ClearedStack/shakedown/internal/spool"
	"github.com/ClearedStack/shakedown/internal/worker"
)

const (
	defaultMusterURL  = "http://muster.clearedstack.svc.cluster.local:8080"
	defaultTokenFile  = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	defaultSpoolDir   = "/var/spool/cs"
	defaultMemoryCap  = 24 << 20
	maxReplaySeedSize = 16 << 20
)

const usage = `usage:
  shakedown check [-json] [-timeout 30s] [-case-timeout 2s] <folder>
  shakedown replay [-timeout 30s] [-case-timeout 2s] [-allow-pause-building] <seed-file>
  shakedown run [-once] [-listen addr] [-poll 2s] [-heartbeat 20s] [-timeout 30s] [-case-timeout 2s]
  shakedown version

exit status: 0 pass, 1 fail, 2 bad usage or unreadable folder,
             3 needs_subtool, 4 timeout
`

func main() {
	if strings.TrimSpace(os.Getenv("GOMEMLIMIT")) == "" {
		debug.SetMemoryLimit(defaultMemoryCap)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(runContext(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func inClusterMode() bool {
	return strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST")) != "" ||
		strings.TrimSpace(os.Getenv("SHAKEDOWN_AUTORUN")) == "1"
}

func run(args []string, stdout, stderr io.Writer) int {
	return runContext(context.Background(), args, stdout, stderr)
}

func runContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		if inClusterMode() {
			return runWorker(ctx, nil, stderr)
		}
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "%s %s (contract %s)\n", check.Component, check.Version, check.Contract)
		return 0
	case "check":
		return runCheck(ctx, args[1:], stdout, stderr)
	case "replay":
		return runReplay(ctx, args[1:], stdout, stderr)
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

func startHealthServer(addr string) (string, func(), error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", nil, errors.New("E_PARSE: health listener could not bind")
	}
	payload, _ := json.Marshal(struct {
		Status    string `json:"status"`
		Component string `json:"component"`
		Version   string `json:"version"`
		Contract  string `json:"contract"`
	}{
		Status:    "ok",
		Component: check.Component,
		Version:   check.Version,
		Contract:  check.Contract,
	})
	payload = append(payload, '\n')

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(payload)
		}
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       2 * time.Second,
		WriteTimeout:      2 * time.Second,
		IdleTimeout:       15 * time.Second,
		MaxHeaderBytes:    4096,
	}
	go func() {
		_ = srv.Serve(ln)
	}()
	stop := func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}
	return ln.Addr().String(), stop, nil
}

func runWorker(ctx context.Context, args []string, stderr io.Writer) int {
	defaultListen := ""
	if inClusterMode() {
		defaultListen = ":8080"
	}

	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	once := fs.Bool("once", false, "drain spool and run at most one lease step")
	listenAddr := fs.String("listen", envOr("SHAKEDOWN_LISTEN_ADDR", defaultListen), "HTTP /healthz listen address (or off)")
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

	sp, inMem, err := spool.OpenOrMemory(*spoolDir)
	if err != nil {
		fmt.Fprintf(stderr, "shakedown: %v\n", err)
		return 2
	}
	if inMem {
		fmt.Fprintln(stderr, "shakedown: spool directory is read-only; using bounded in-memory spool")
	}

	addr := strings.TrimSpace(*listenAddr)
	if addr != "" && !strings.EqualFold(addr, "off") && !strings.EqualFold(addr, "none") {
		_, stopHealth, err := startHealthServer(addr)
		if err != nil {
			fmt.Fprintf(stderr, "shakedown: %v\n", err)
			return 2
		}
		defer stopHealth()
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

type replayItemReport struct {
	ID        string          `json:"id"`
	State     string          `json:"state"`
	Held      bool            `json:"held,omitempty"`
	Next      string          `json:"next,omitempty"`
	Shakedown json.RawMessage `json:"shakedown,omitempty"`
}

type replayReport struct {
	Component string             `json:"component"`
	Version   string             `json:"version"`
	Contract  string             `json:"contract"`
	Processed int                `json:"processed"`
	Items     []replayItemReport `json:"items"`
}

type inMemoryResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (w *inMemoryResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *inMemoryResponseWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}

func (w *inMemoryResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}

type handlerRoundTripper struct {
	handler http.Handler
}

func (rt handlerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	rec := &inMemoryResponseWriter{}
	rt.handler.ServeHTTP(rec, req)
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode:    status,
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:        rec.Header(),
		Body:          io.NopCloser(bytes.NewReader(rec.body.Bytes())),
		ContentLength: int64(rec.body.Len()),
		Request:       req,
	}, nil
}

func decodeSeedItems(raw []byte) ([]muster.SeedItem, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("seed file contains no items")
	}
	if trimmed[0] == '[' {
		var items []muster.SeedItem
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&items); err != nil {
			return nil, errors.New("seed file is not valid JSON")
		}
		if _, err := dec.Token(); err != io.EOF {
			return nil, errors.New("seed file has trailing JSON tokens")
		}
		if len(items) == 0 {
			return nil, errors.New("seed file contains no items")
		}
		return items, nil
	}

	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	var items []muster.SeedItem
	for {
		var item muster.SeedItem
		err := dec.Decode(&item)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("seed file is not valid JSONL")
		}
		items = append(items, item)
	}
	if len(items) == 0 {
		return nil, errors.New("seed file contains no items")
	}
	return items, nil
}

func runReplay(parent context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	timeout := fs.Duration("timeout", worker.DefaultCheckTimeout, "deadline for each bundle check")
	caseTimeout := fs.Duration("case-timeout", worker.DefaultCaseTimeout, "deadline for each worked example")
	allowPause := fs.Bool("allow-pause-building", false, "allow StandIn to pause building items on needs_subtool")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || *timeout <= 0 || *caseTimeout <= 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}

	f, err := os.Open(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "shakedown: %s: the seed file could not be read\n", check.EParse)
		return 2
	}
	raw, readErr := io.ReadAll(io.LimitReader(f, maxReplaySeedSize+1))
	_ = f.Close()
	if readErr != nil || len(raw) > maxReplaySeedSize {
		fmt.Fprintf(stderr, "shakedown: %s: the seed file could not be read\n", check.EParse)
		return 2
	}

	items, err := decodeSeedItems(raw)
	if err != nil {
		fmt.Fprintf(stderr, "shakedown: %s: %v\n", check.EParse, err)
		return 2
	}

	standIn := muster.NewStandIn()
	standIn.SetAllowPauseBuilding(*allowPause)
	if err := standIn.Seed(items); err != nil {
		fmt.Fprintf(stderr, "shakedown: %s: seed items rejected by contract stand-in\n", check.EParse)
		return 2
	}

	cli := &muster.Client{
		BaseURL: "http://muster.local",
		Token:   muster.StaticTokenSource(standIn.TokenFor("shakedown")),
		HTTPClient: &http.Client{
			Transport: handlerRoundTripper{handler: standIn},
		},
	}
	w, err := worker.New(worker.Config{
		Dispatcher:        cli,
		Spool:             spool.NewMemory(),
		CheckTimeout:      *timeout,
		CaseTimeout:       *caseTimeout,
		HeartbeatInterval: worker.DefaultHeartbeatInterval,
		Log:               io.Discard,
	})
	if err != nil {
		fmt.Fprintf(stderr, "shakedown: %v\n", err)
		return 2
	}

	processed := 0
	for i := 0; i < len(items); i++ {
		worked, stepErr := w.Step(parent)
		if stepErr != nil {
			if parent.Err() != nil {
				fmt.Fprintf(stderr, "shakedown: %s: replay timed out\n", muster.CodeTimeout)
				return 4
			}
			fmt.Fprintf(stderr, "shakedown: %s: replay step failed\n", muster.CodeParse)
			return 1
		}
		if !worked {
			break
		}
		processed++
	}

	out := replayReport{
		Component: check.Component,
		Version:   check.Version,
		Contract:  check.Contract,
		Processed: processed,
		Items:     make([]replayItemReport, 0, len(items)),
	}
	for _, seed := range items {
		rec, _, held := standIn.Item(seed.ID)
		out.Items = append(out.Items, replayItemReport{
			ID:        seed.ID,
			State:     rec.State,
			Held:      held,
			Next:      standIn.NextWorker(seed.ID),
			Shakedown: rec.Results["shakedown"],
		})
	}

	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return 2
	}
	return 0
}
