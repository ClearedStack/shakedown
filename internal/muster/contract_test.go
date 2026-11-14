package muster

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const contractExamplesDir = "../../testdata/contract/examples"

type savedExample struct {
	About   string       `json:"about"`
	Given   *savedGiven  `json:"given,omitempty"`
	Request savedRequest `json:"request"`
	Expect  savedExpect  `json:"expect"`
}

type savedGiven struct {
	Items     []SeedItem     `json:"items,omitempty"`
	Calls     []savedRequest `json:"calls,omitempty"`
	Wait      string         `json:"wait,omitempty"`
	Condition string         `json:"condition,omitempty"`
}

type savedRequest struct {
	As          string          `json:"as,omitempty"`
	Method      string          `json:"method"`
	Path        string          `json:"path"`
	ContentType *string         `json:"content_type,omitempty"`
	Body        json.RawMessage `json:"body,omitempty"`
	Raw         *string         `json:"raw,omitempty"`
}

type savedExpect struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body,omitempty"`
	Stream *string         `json:"stream,omitempty"`
}

func doSavedRequest(t *testing.T, s *StandIn, r savedRequest) *httptest.ResponseRecorder {
	t.Helper()
	var body []byte
	switch {
	case r.Raw != nil:
		body = []byte(*r.Raw)
	case r.Body != nil:
		var buf bytes.Buffer
		if err := json.Compact(&buf, r.Body); err != nil {
			t.Fatalf("compact request body: %v", err)
		}
		body = buf.Bytes()
	}
	req := httptest.NewRequest(r.Method, r.Path, bytes.NewReader(body))
	if body != nil {
		ct := "application/json"
		if r.ContentType != nil {
			ct = *r.ContentType
		}
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
	}
	if r.As != "" {
		tok := s.TokenFor(r.As)
		if tok == "" {
			tok = "unknown-caller-" + r.As
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func openSavedFeed(t *testing.T, s *StandIn, r savedRequest) (int, []byte) {
	t.Helper()
	srv := httptest.NewServer(s)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, r.Method, srv.URL+r.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.As != "" {
		req.Header.Set("Authorization", "Bearer "+s.TokenFor(r.As))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	wantEvents := 1 + s.ComponentCount(ctx)
	var out bytes.Buffer
	reader := bufio.NewReader(resp.Body)
	for events := 0; events < wantEvents; {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("feed ended early: %v", err)
		}
		out.WriteString(line)
		if line == "\n" {
			events++
		}
	}
	return http.StatusOK, out.Bytes()
}

// TestReplayAllContractExamples replays every saved example from
// ClearedStack/muster contract/v1/examples/ against StandIn and checks exact
// status codes, JSON responses, SSE frames, paths, and all 28 refusals.
func TestReplayAllContractExamples(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(contractExamplesDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 52 {
		t.Fatalf("found %d contract examples, want 52", len(paths))
	}
	sort.Strings(paths)

	seenPaths := map[string]bool{}
	seenRefusals := map[*problem]bool{}

	for _, p := range paths {
		name := strings.TrimSuffix(filepath.Base(p), ".json")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			var ex savedExample
			if err := decodeStrict(raw, &ex); err != nil {
				t.Fatalf("%s: %v", p, err)
			}
			if ex.About == "" {
				t.Fatal("missing about field")
			}

			s := NewStandIn()
			if g := ex.Given; g != nil {
				if len(g.Items) > 0 {
					if err := s.Seed(g.Items); err != nil {
						t.Fatalf("seed: %v", err)
					}
				}
				for i, c := range g.Calls {
					rec := doSavedRequest(t, s, c)
					if rec.Code < 200 || rec.Code > 299 {
						t.Fatalf("given call %d (%s) returned %d: %s", i+1, c.Path, rec.Code, rec.Body.String())
					}
				}
				if g.Wait != "" {
					d, err := time.ParseDuration(g.Wait)
					if err != nil {
						t.Fatal(err)
					}
					s.AdvanceClock(d)
				}
				if err := s.Arrange(g.Condition); err != nil {
					t.Fatal(err)
				}
			}

			seenPaths[ex.Request.Path] = true

			var status int
			var body []byte
			if ex.Expect.Stream != nil {
				status, body = openSavedFeed(t, s, ex.Request)
				if status != ex.Expect.Status || string(body) != *ex.Expect.Stream {
					t.Errorf("feed mismatch:\ngot  %d:\n%s\nwant %d:\n%s", status, body, ex.Expect.Status, *ex.Expect.Stream)
				}
			} else {
				rec := doSavedRequest(t, s, ex.Request)
				status, body = rec.Code, rec.Body.Bytes()
				var want []byte
				if ex.Expect.Body != nil {
					var buf bytes.Buffer
					if err := json.Compact(&buf, ex.Expect.Body); err != nil {
						t.Fatalf("compact expect.body: %v", err)
					}
					want = append(buf.Bytes(), '\n')
				}
				if status != ex.Expect.Status || !bytes.Equal(body, want) {
					t.Errorf("response mismatch:\ngot  %d %s\nwant %d %s", status, body, ex.Expect.Status, want)
				}
			}

			if status >= 400 {
				var r struct {
					Error  string `json:"error"`
					Cause  string `json:"cause"`
					Reason string `json:"reason"`
				}
				if err := decodeStrict(body, &r); err != nil {
					t.Fatalf("refusal is not strict JSON: %v (%s)", err, body)
				}
				for _, prob := range allProblems {
					if prob.Status == status && prob.Code == r.Error && prob.Cause == r.Cause && prob.Reason == r.Reason {
						seenRefusals[prob] = true
					}
				}
			}
		})
	}

	for routePath := range standInRoutes {
		if !seenPaths[routePath] {
			t.Errorf("no saved example exercised %s", routePath)
		}
	}
	for _, prob := range allProblems {
		if !seenRefusals[prob] {
			t.Errorf("no saved example exercised refusal %d %s %q", prob.Status, prob.Cause, prob.Reason)
		}
	}
}

// TestReplayWorkerExamplesThroughClient replays every saved worker example and
// every saved refusal example from contract/v1/examples/ through muster.Client.
func TestReplayWorkerExamplesThroughClient(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(contractExamplesDir, "*.json"))
	if err != nil || len(paths) != 52 {
		t.Fatalf("expected 52 examples, got %d (%v)", len(paths), err)
	}
	sort.Strings(paths)

	replayedSuccess := 0
	replayedRefusals := 0

	for _, p := range paths {
		name := strings.TrimSuffix(filepath.Base(p), ".json")
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var ex savedExample
		if err := decodeStrict(raw, &ex); err != nil {
			t.Fatal(err)
		}

		// 1. Replay every refusal response through Client to verify strict
		// Refusal decoding and Retryable classification.
		if ex.Expect.Status >= 400 {
			t.Run("refusal/"+name, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var buf bytes.Buffer
					_ = json.Compact(&buf, ex.Expect.Body)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(ex.Expect.Status)
					_, _ = w.Write(append(buf.Bytes(), '\n'))
				}))
				defer srv.Close()

				cli := NewClient(srv.URL, StaticTokenSource("synthetic-token"))
				_, callErr := cli.Lease(context.Background())
				var ref *Refusal
				if !errors.As(callErr, &ref) {
					t.Fatalf("expected *Refusal, got %T (%v)", callErr, callErr)
				}
				var want struct {
					Error  string `json:"error"`
					Cause  string `json:"cause"`
					Reason string `json:"reason"`
				}
				if err := json.Unmarshal(ex.Expect.Body, &want); err != nil {
					t.Fatal(err)
				}
				if ref.Status != ex.Expect.Status || ref.Code != want.Error || ref.Cause != want.Cause || ref.Reason != want.Reason {
					t.Errorf("got %+v, want %d %+v", ref, ex.Expect.Status, want)
				}
				wantRetry := want.Error == CodeTimeout || ex.Expect.Status == http.StatusServiceUnavailable || ex.Expect.Status == http.StatusTooManyRequests
				if ref.Retryable() != wantRetry {
					t.Errorf("Retryable() = %v, want %v", ref.Retryable(), wantRetry)
				}
				if strings.Contains(ref.Error(), srv.URL) || strings.Contains(ref.Error(), "127.0.0.1") {
					t.Errorf("refusal error leaked server address: %s", ref.Error())
				}
			})
			replayedRefusals++
		}

		// 2. Replay every 2xx worker call example end-to-end through Client
		// against StandIn.
		switch ex.Request.Path {
		case "/v1/lease", "/v1/heartbeat", "/v1/complete", "/v1/fail", "/v1/needs_subtool", "/v1/release":
		default:
			continue
		}
		if ex.Expect.Status < 200 || ex.Expect.Status > 299 {
			continue
		}

		t.Run("worker/"+name, func(t *testing.T) {
			s := NewStandIn()
			if g := ex.Given; g != nil {
				if len(g.Items) > 0 {
					if err := s.Seed(g.Items); err != nil {
						t.Fatal(err)
					}
				}
				for _, c := range g.Calls {
					rec := doSavedRequest(t, s, c)
					if rec.Code < 200 || rec.Code > 299 {
						t.Fatalf("given call failed: %d", rec.Code)
					}
				}
				if g.Wait != "" {
					d, _ := time.ParseDuration(g.Wait)
					s.AdvanceClock(d)
				}
				if err := s.Arrange(g.Condition); err != nil {
					t.Fatal(err)
				}
			}

			srv := httptest.NewServer(s)
			defer srv.Close()

			cli := NewClient(srv.URL, StaticTokenSource(s.TokenFor(ex.Request.As)))
			ctx := context.Background()

			var gotJSON []byte
			switch ex.Request.Path {
			case "/v1/lease":
				grant, err := cli.Lease(ctx)
				if err != nil {
					t.Fatalf("Lease: %v", err)
				}
				if ex.Expect.Status == http.StatusNoContent {
					if grant != nil {
						t.Fatalf("expected nil grant on 204, got %+v", grant)
					}
					return
				}
				gotJSON, _ = json.Marshal(grant)
			case "/v1/heartbeat":
				var req leaseIDWire
				_ = json.Unmarshal(ex.Request.Body, &req)
				beat, err := cli.Heartbeat(ctx, req.Lease)
				if err != nil {
					t.Fatalf("Heartbeat: %v", err)
				}
				gotJSON, _ = json.Marshal(beat)
			case "/v1/complete":
				var req completeWire
				_ = json.Unmarshal(ex.Request.Body, &req)
				ack, err := cli.Complete(ctx, req.Lease, req.State, req.Result)
				if err != nil {
					t.Fatalf("Complete: %v", err)
				}
				gotJSON, _ = json.Marshal(ack)
			case "/v1/fail":
				var req failWire
				_ = json.Unmarshal(ex.Request.Body, &req)
				ack, err := cli.Fail(ctx, req.Lease, req.Code)
				if err != nil {
					t.Fatalf("Fail: %v", err)
				}
				gotJSON, _ = json.Marshal(ack)
			case "/v1/needs_subtool":
				var req leaseIDWire
				_ = json.Unmarshal(ex.Request.Body, &req)
				ack, err := cli.NeedsSubtool(ctx, req.Lease)
				if err != nil {
					t.Fatalf("NeedsSubtool: %v", err)
				}
				gotJSON, _ = json.Marshal(ack)
			case "/v1/release":
				var req releaseWire
				_ = json.Unmarshal(ex.Request.Body, &req)
				ack, err := cli.Release(ctx, req.ID)
				if err != nil {
					t.Fatalf("Release: %v", err)
				}
				gotJSON, _ = json.Marshal(ack)
			}

			var wantBuf bytes.Buffer
			if err := json.Compact(&wantBuf, ex.Expect.Body); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(gotJSON, wantBuf.Bytes()) {
				t.Errorf("client decoded mismatch:\ngot  %s\nwant %s", gotJSON, wantBuf.Bytes())
			}
		})
		replayedSuccess++
	}

	if replayedSuccess == 0 || replayedRefusals == 0 {
		t.Fatalf("replayedSuccess=%d replayedRefusals=%d", replayedSuccess, replayedRefusals)
	}
}

func TestClientSanitisesErrorsAndRotatesToken(t *testing.T) {
	tokFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokFile, []byte("first-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var seenAuth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = append(seenAuth, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	cli := NewClient(srv.URL, FileTokenSource(tokFile))
	if _, err := cli.Lease(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokFile, []byte("rotated-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Lease(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(seenAuth) != 2 || seenAuth[0] != "Bearer first-token" || seenAuth[1] != "Bearer rotated-token" {
		t.Fatalf("unexpected auth headers: %v", seenAuth)
	}

	// Oversized request rejected before leaving the client.
	huge := `{"pad":"` + strings.Repeat("a", MaxBody) + `"}`
	_, err := cli.Complete(context.Background(), "PA-0001.1.1", "building", json.RawMessage(huge))
	if err == nil || !strings.HasPrefix(err.Error(), CodeParse) {
		t.Fatalf("expected E_PARSE on >64KiB body, got %v", err)
	}

	// Unreachable server must never leak URL, host, or port.
	srv.Close()
	_, err = cli.Lease(context.Background())
	if err == nil || !IsRetryable(err) {
		t.Fatalf("expected retryable transport error, got %v", err)
	}
	if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "http") {
		t.Fatalf("transport error leaked address: %s", err.Error())
	}

	// Missing token file must never leak path.
	badCli := NewClient("http://127.0.0.1:9", FileTokenSource(filepath.Join(t.TempDir(), "missing-secret")))
	_, err = badCli.Lease(context.Background())
	if err == nil || strings.Contains(err.Error(), "missing-secret") {
		t.Fatalf("token error leaked file path: %v", err)
	}
}
