package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClearedStack/shakedown/internal/muster"
)

const tools = "../../testdata/tools"

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

	files := map[string]string{}
	root := filepath.Join(tools, "passing-tool")
	if err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(raw)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
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
