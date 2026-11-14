package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/shakedown/internal/muster"
)

const tools = "../../testdata/tools"

func loadToolFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	if err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(raw)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return files
}

func TestExitStatus(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"pass", []string{"check", filepath.Join(tools, "passing-tool")}, 0},
		{"fail", []string{"check", filepath.Join(tools, "failing-tool")}, 1},
		{"json", []string{"check", "-json", filepath.Join(tools, "passing-tool")}, 0},
		{"missing folder", []string{"check", filepath.Join(t.TempDir(), "not-here")}, 2},
		{"no folder", []string{"check"}, 2},
		{"replay no file", []string{"replay"}, 2},
		{"unknown command", []string{"publish"}, 2},
		{"no arguments", nil, 2},
		{"version", []string{"version"}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if got := run(c.args, &out, &errOut); got != c.want {
				t.Errorf("exit status %d, want %d\nstdout: %s\nstderr: %s", got, c.want, &out, &errOut)
			}
		})
	}
}

func TestPrintsEachCondition(t *testing.T) {
	var out, errOut bytes.Buffer
	run([]string{"check", filepath.Join(tools, "failing-tool")}, &out, &errOut)
	for _, line := range []string{"offline    fail", "csp        fail", "tracking   fail", "examples   fail", "citations  fail", "verdict    fail"} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("output lacks %q:\n%s", line, &out)
		}
	}
}

func TestMissingFolderErrorIsSanitised(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-here")
	var out, errOut bytes.Buffer
	run([]string{"check", dir}, &out, &errOut)
	if got := errOut.String(); got != "shakedown: E_PARSE: the folder could not be read\n" {
		t.Errorf("stderr was %q", got)
	}
}

func TestRunOnceAgainstMusterStandIn(t *testing.T) {
	standIn := muster.NewStandIn()
	srv := httptest.NewServer(standIn)
	defer srv.Close()

	files := loadToolFiles(t, filepath.Join(tools, "passing-tool"))
	moldloftBytes, _ := json.Marshal(map[string]any{"files": files})
	if err := standIn.Seed([]muster.SeedItem{{
		ID:      "PA-0001",
		State:   "building",
		Lane:    "normal",
		Next:    "shakedown",
		Results: map[string]json.RawMessage{"moldloft": moldloftBytes},
	}}); err != nil {
		t.Fatal(err)
	}

	tokPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokPath, []byte(standIn.TokenFor("shakedown")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spoolDir := t.TempDir()

	var out, errOut bytes.Buffer
	code := run([]string{
		"run",
		"-once",
		"-listen", "off",
		"-muster-url", srv.URL,
		"-token-file", tokPath,
		"-spool-dir", spoolDir,
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit status %d, want 0; stderr=%s", code, errOut.String())
	}
	if got := standIn.NextWorker("PA-0001"); got != "chartroom" {
		t.Fatalf("next worker = %q, want chartroom", got)
	}

	// Unknown contract version returns exit status 3 (E_NEEDS_SUBTOOL).
	t.Setenv("CONTRACT_VERSION", "v99")
	errOut.Reset()
	if got := run([]string{"run", "-once", "-spool-dir", spoolDir}, &out, &errOut); got != 3 {
		t.Fatalf("exit status %d, want 3 on unknown contract version", got)
	}
}

func TestHealthServerEndpoint(t *testing.T) {
	addr, stop, err := startHealthServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", resp.StatusCode)
	}
	var status struct {
		Status    string `json:"status"`
		Component string `json:"component"`
		Contract  string `json:"contract"`
	}
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatalf("invalid /healthz JSON: %v", err)
	}
	if status.Status != "ok" || status.Component != "shakedown" || status.Contract != "v1" {
		t.Fatalf("unexpected /healthz payload: %+v", status)
	}

	headResp, err := http.Head("http://" + addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	headResp.Body.Close()
	if headResp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD /healthz status = %d, want 200", headResp.StatusCode)
	}

	postResp, err := http.Post("http://"+addr+"/healthz", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	postResp.Body.Close()
	if postResp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /healthz status = %d, want 405", postResp.StatusCode)
	}

	notFound, err := http.Get("http://" + addr + "/other")
	if err != nil {
		t.Fatal(err)
	}
	notFound.Body.Close()
	if notFound.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /other status = %d, want 404", notFound.StatusCode)
	}
}

func TestZeroArgInClusterAutorunWithReadOnlySpool(t *testing.T) {
	standIn := muster.NewStandIn()
	srv := httptest.NewServer(standIn)
	defer srv.Close()

	files := loadToolFiles(t, filepath.Join(tools, "passing-tool"))
	moldloftBytes, _ := json.Marshal(map[string]any{"files": files})
	if err := standIn.Seed([]muster.SeedItem{{
		ID:      "PA-0001",
		State:   "building",
		Lane:    "normal",
		Next:    "shakedown",
		Results: map[string]json.RawMessage{"moldloft": moldloftBytes},
	}}); err != nil {
		t.Fatal(err)
	}

	tokPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokPath, []byte(standIn.TokenFor("shakedown")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	roParent := filepath.Join(t.TempDir(), "ro")
	if err := os.MkdirAll(roParent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roParent, 0o700) })

	t.Setenv("SHAKEDOWN_AUTORUN", "1")
	t.Setenv("MUSTER_URL", srv.URL)
	t.Setenv("SHAKEDOWN_TOKEN_FILE", tokPath)
	t.Setenv("SHAKEDOWN_SPOOL_DIR", filepath.Join(roParent, "spool"))
	t.Setenv("SHAKEDOWN_LISTEN_ADDR", "127.0.0.1:0")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan int, 1)
	var out, errOut bytes.Buffer
	go func() {
		done <- runContext(ctx, nil, &out, &errOut)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if standIn.NextWorker("PA-0001") == "chartroom" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()

	code := <-done
	if code != 0 {
		t.Fatalf("autorun exit status %d, want 0; stderr=%s", code, errOut.String())
	}
	if got := standIn.NextWorker("PA-0001"); got != "chartroom" {
		t.Fatalf("next worker = %q, want chartroom", got)
	}
	if !strings.Contains(errOut.String(), "using bounded in-memory spool") {
		t.Fatalf("expected in-memory spool fallback notice, got %q", errOut.String())
	}
}

func TestReplaySubcommand(t *testing.T) {
	passFiles := loadToolFiles(t, filepath.Join(tools, "passing-tool"))
	failFiles := loadToolFiles(t, filepath.Join(tools, "failing-tool"))

	passMoldloft, _ := json.Marshal(map[string]any{"files": passFiles})
	failMoldloft, _ := json.Marshal(map[string]any{"files": failFiles})
	parkMoldloft, _ := json.Marshal(map[string]any{"regulatory_ambiguity": true})

	seeds := []muster.SeedItem{
		{
			ID:      "PA-0001",
			State:   "building",
			Lane:    "normal",
			Next:    "shakedown",
			Results: map[string]json.RawMessage{"moldloft": passMoldloft},
		},
		{
			ID:      "PA-0002",
			State:   "building",
			Lane:    "normal",
			Next:    "shakedown",
			Results: map[string]json.RawMessage{"moldloft": failMoldloft},
		},
		{
			ID:      "PA-0003",
			State:   "building",
			Lane:    "normal",
			Next:    "shakedown",
			Results: map[string]json.RawMessage{"moldloft": parkMoldloft},
		},
	}

	// Test JSONL format.
	var jsonlBuf bytes.Buffer
	for _, s := range seeds {
		line, _ := json.Marshal(s)
		jsonlBuf.Write(line)
		jsonlBuf.WriteByte('\n')
	}
	seedFile := filepath.Join(t.TempDir(), "seeds.jsonl")
	if err := os.WriteFile(seedFile, jsonlBuf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := run([]string{"replay", seedFile}, &out, &errOut); code != 0 {
		t.Fatalf("replay exit status %d, want 0; stderr=%s", code, errOut.String())
	}

	var rep replayReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("replay output invalid JSON: %v\n%s", err, out.String())
	}
	if rep.Processed != 3 || len(rep.Items) != 3 {
		t.Fatalf("processed=%d items=%d, want 3/3", rep.Processed, len(rep.Items))
	}
	if rep.Items[0].Next != "chartroom" || rep.Items[0].Held {
		t.Errorf("PA-0001 = %+v, want next=chartroom held=false", rep.Items[0])
	}
	if !rep.Items[1].Held || rep.Items[1].Next != "" {
		t.Errorf("PA-0002 = %+v, want held=true", rep.Items[1])
	}
	if rep.Items[2].State != "parked" || rep.Items[2].Next != "" {
		t.Errorf("PA-0003 = %+v, want state=parked", rep.Items[2])
	}

	// Missing seed file error is sanitized and leaks no path.
	missing := filepath.Join(t.TempDir(), "secret-seed.json")
	out.Reset()
	errOut.Reset()
	if code := run([]string{"replay", missing}, &out, &errOut); code != 2 {
		t.Fatalf("missing seed exit status %d, want 2", code)
	}
	if strings.Contains(errOut.String(), "secret-seed") {
		t.Fatalf("stderr leaked path: %q", errOut.String())
	}
}
