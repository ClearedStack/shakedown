package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
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
