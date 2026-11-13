// Package check runs the five listing conditions against a folder of static
// files. It is deterministic: no clock, no randomness, no network, no model.
package check

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Names and versions. They mirror component.json; a test keeps them in step.
const (
	Component = "shakedown"
	Version   = "v0.1.0-draft"
	Contract  = "v1"
)

// Code is one of the three public error codes.
type Code string

const (
	EParse        Code = "E_PARSE"
	ETimeout      Code = "E_TIMEOUT"
	ENeedsSubtool Code = "E_NEEDS_SUBTOOL"
)

// Status is the outcome of a condition, or of the whole bundle.
type Status string

const (
	Pass         Status = "pass"
	Fail         Status = "fail"
	Timeout      Status = "timeout"
	NeedsSubtool Status = "needs_subtool"
)

// The five listing conditions, in the order the specification gives them.
const (
	Offline   = "offline"
	CSP       = "csp"
	Tracking  = "tracking"
	Examples  = "examples"
	Citations = "citations"

	// wholeBundle marks a finding that applies to every condition.
	wholeBundle = "bundle"
)

var conditionOrder = []string{Offline, CSP, Tracking, Examples, Citations}

// Finding is one reason a condition did not pass. Messages are built only
// from fixed text and from bundle-relative names that passed the name rules,
// so they cannot carry a machine name, an address or a credential.
type Finding struct {
	Condition string `json:"condition"`
	Rule      string `json:"rule"`
	Code      Code   `json:"code"`
	File      string `json:"file,omitempty"`
	Message   string `json:"message"`
}

// ConditionResult is the pass or fail for one condition.
type ConditionResult struct {
	Condition string `json:"condition"`
	Status    Status `json:"status"`
}

// Report is the full result for one bundle.
type Report struct {
	Component  string            `json:"component"`
	Version    string            `json:"version"`
	Contract   string            `json:"contract"`
	Verdict    Status            `json:"verdict"`
	Conditions []ConditionResult `json:"conditions"`
	Findings   []Finding         `json:"findings"`
}

func statusOf(c Code) Status {
	switch c {
	case ETimeout:
		return Timeout
	case ENeedsSubtool:
		return NeedsSubtool
	default:
		return Fail
	}
}

// rank orders statuses: a definite failure outranks a timeout, which
// outranks an unfamiliar case, which outranks a pass.
func rank(s Status) int {
	switch s {
	case Fail:
		return 3
	case Timeout:
		return 2
	case NeedsSubtool:
		return 1
	default:
		return 0
	}
}

func worse(a, b Status) Status {
	if rank(b) > rank(a) {
		return b
	}
	return a
}

func conditionIndex(name string) int {
	if name == wholeBundle {
		return -1
	}
	for i, c := range conditionOrder {
		if c == name {
			return i
		}
	}
	return len(conditionOrder)
}

func buildReport(findings []Finding) Report {
	seen := map[Finding]bool{}
	out := []Finding{}
	for _, f := range findings {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ai, bi := conditionIndex(a.Condition), conditionIndex(b.Condition); ai != bi {
			return ai < bi
		}
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		return a.Message < b.Message
	})

	r := Report{Component: Component, Version: Version, Contract: Contract, Verdict: Pass, Findings: out}
	status := map[string]Status{}
	for _, c := range conditionOrder {
		status[c] = Pass
	}
	for _, f := range out {
		s := statusOf(f.Code)
		if f.Condition == wholeBundle {
			for _, c := range conditionOrder {
				status[c] = worse(status[c], s)
			}
			continue
		}
		status[f.Condition] = worse(status[f.Condition], s)
	}
	for _, c := range conditionOrder {
		r.Conditions = append(r.Conditions, ConditionResult{Condition: c, Status: status[c]})
		r.Verdict = worse(r.Verdict, status[c])
	}
	return r
}

// WriteText prints the report for a person to read.
func (r Report) WriteText(w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s (contract %s)\n\n", r.Component, r.Version, r.Contract)
	for _, c := range r.Conditions {
		fmt.Fprintf(&b, "%-10s %s\n", c.Condition, c.Status)
	}
	fmt.Fprintf(&b, "\n%-10s %s\n", "verdict", r.Verdict)
	if len(r.Findings) > 0 {
		b.WriteString("\nfindings:\n")
		for _, f := range r.Findings {
			where := ""
			if f.File != "" {
				where = f.File + ": "
			}
			fmt.Fprintf(&b, "  %-9s %-15s %-26s %s%s\n", f.Condition, f.Code, f.Rule, where, f.Message)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// WriteJSON prints the report for a machine to read.
func (r Report) WriteJSON(w io.Writer) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}
