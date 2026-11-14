package check

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/ClearedStack/shakedown/internal/js"
)

const examplesFormat = "v1"

var exampleKinds = map[string]bool{"worked": true, "boundary": true, "synthetic": true}

type exampleCase struct {
	name   string
	input  json.RawMessage
	expect any
}

// runExamples returns the calculations or cited rules the examples exercise,
// and whether the examples file could be read at all.
func (c *checker) runExamples(b *bundle, m *manifest, pages map[string]*page) (map[string]bool, bool) {
	if m == nil {
		return nil, false
	}
	if m.Kind == manifestMoldloft {
		return c.runMoldloftExamples(b, m, pages)
	}
	p := m.Examples
	bad := func(msg string) (map[string]bool, bool) {
		c.add(Examples, "EX-FILE", EParse, p, msg)
		return nil, false
	}
	data, ok := b.data[p]
	if !ok {
		return nil, false // already reported as too large
	}
	var top map[string]json.RawMessage
	if decodeStrict(data, &top) != nil || top == nil {
		return bad("not a JSON object")
	}
	var format string
	if top["format"] == nil || json.Unmarshal(top["format"], &format) != nil {
		return bad("format is required")
	}
	if format != examplesFormat || !onlyKeys(top, "format", "cases") {
		c.add(Examples, "EX-FORMAT", ENeedsSubtool, p, "no rule covers this examples format")
		return nil, false
	}
	var raw []map[string]json.RawMessage
	if top["cases"] == nil || json.Unmarshal(top["cases"], &raw) != nil {
		return bad("cases must be a list of objects")
	}

	calcs := map[string]bool{}
	kinds := map[string]bool{}
	names := map[string]bool{}
	var cases []exampleCase
	for _, r := range raw {
		var name, kind, calc string
		str := func(key string, dst *string) bool { return r[key] != nil && json.Unmarshal(r[key], dst) == nil }
		if !str("name", &name) || !str("kind", &kind) || !str("calculation", &calc) || r["input"] == nil || r["expect"] == nil {
			return bad("every case needs name, kind, calculation, input and expect")
		}
		if !idRE.MatchString(name) || !idRE.MatchString(calc) || names[name] {
			return bad("case names must be unique, and names and calculations lower-case letters, digits and dashes")
		}
		if !exampleKinds[kind] || !onlyKeys(r, "name", "kind", "calculation", "input", "expect") {
			c.add(Examples, "EX-FORMAT", ENeedsSubtool, p, "no rule covers a kind or field in a case")
			return nil, false
		}
		var expect any
		if json.Unmarshal(r["expect"], &expect) != nil {
			return bad("expect is not valid JSON")
		}
		names[name], kinds[kind], calcs[calc] = true, true, true
		cases = append(cases, exampleCase{name: name, input: r["input"], expect: expect})
	}
	if !kinds["worked"] || !kinds["boundary"] {
		c.add(Examples, "EX-COVERAGE", EParse, p, "the examples need at least one worked case and one boundary case")
	}

	var progs []*js.Program
	for _, s := range m.Scripts {
		prog, err := js.Parse(s, string(b.data[s]))
		if err != nil {
			c.add(Examples, "EX-SCRIPT", EParse, s, "the logic script could not be loaded")
			return calcs, true
		}
		progs = append(progs, prog)
	}

	// Each case runs in a fresh engine so that no case can lean on another.
	for i, ec := range cases {
		if c.done() {
			break
		}
		out, err := c.runCase(progs, m.Function, ec.input)
		interrupted := errors.Is(err, js.ErrInterrupted)
		switch {
		case interrupted:
			c.add(Examples, "EX-TIMEOUT", ETimeout, p, "the case "+ec.name+" did not finish before its deadline")
		case errors.Is(err, errLoad):
			c.add(Examples, "EX-SCRIPT", EParse, m.Scripts[0], "the logic scripts failed while loading")
		case errors.Is(err, errNoFunction):
			c.add(Examples, "EX-SCRIPT", EParse, manifestName, "the logic scripts do not define the named function")
		case err != nil:
			c.add(Examples, "EX-THREW", EParse, p, "the case "+ec.name+" raised an error")
		default:
			var got any
			if out == "" || json.Unmarshal([]byte(out), &got) != nil || !reflect.DeepEqual(got, ec.expect) {
				c.add(Examples, "EX-MISMATCH", EParse, p, "the case "+ec.name+" did not give the expected result")
			}
		}
		if i == 0 && err != nil && (errors.Is(err, errLoad) || errors.Is(err, errNoFunction)) {
			break // the same failure would repeat for every case
		}
		if interrupted {
			break // leave the item retryable instead of spending every deadline
		}
	}
	return calcs, true
}

const moldloftRunnerSrc = `
function __shakedownRunMoldloftCase(example) {
  if (!globalThis.MoldloftCheck || typeof globalThis.MoldloftCheck.run !== "function" ||
      !globalThis.MoldloftEngine || typeof globalThis.MoldloftEngine.evaluate !== "function") {
    return null;
  }
  return globalThis.MoldloftCheck.run(globalThis.MOLDLOFT_TOOL, [example], globalThis.MoldloftEngine);
}
`

type moldloftCase struct {
	id         string
	rawJSON    json.RawMessage
	answersMap map[string]string
	inputStr   string
	expectMap  map[string]any
}

type moldloftCheckResult struct {
	ID       string   `json:"id"`
	OK       bool     `json:"ok"`
	Problems []string `json:"problems"`
}

func (c *checker) runMoldloftExamples(b *bundle, m *manifest, pages map[string]*page) (map[string]bool, bool) {
	p := m.Examples
	bad := func(msg string) (map[string]bool, bool) {
		c.add(Examples, "EX-FILE", EParse, p, msg)
		return nil, false
	}
	data, ok := b.data[p]
	if !ok {
		return nil, false
	}
	var rawList []json.RawMessage
	if decodeStrict(data, &rawList) != nil || rawList == nil {
		return bad("worked-examples.json must be a JSON list")
	}
	if len(rawList) == 0 {
		c.add(Examples, "EX-COVERAGE", EParse, p, "the examples need at least one worked case")
		return map[string]bool{}, true
	}

	inputKey := "inputs"
	switch m.Template {
	case "checklist", "decision-tree":
		inputKey = "answers"
	case "validator":
		inputKey = "input"
	}

	seenIDs := map[string]bool{}
	var cases []moldloftCase
	for _, rawElem := range rawList {
		var r map[string]json.RawMessage
		if decodeStrict(rawElem, &r) != nil || r == nil {
			return bad("every worked example must be a JSON object")
		}
		if !onlyKeys(r, "id", "label", inputKey, "expect") {
			c.add(Examples, "EX-FORMAT", ENeedsSubtool, p, "no rule covers a field in a worked example")
			return nil, false
		}
		var id, label string
		if r["id"] == nil || json.Unmarshal(r["id"], &id) != nil || !moldloftIDRE.MatchString(id) || seenIDs[id] ||
			r["label"] == nil || json.Unmarshal(r["label"], &label) != nil || label == "" ||
			r[inputKey] == nil || r["expect"] == nil {
			return bad("every worked example needs a unique id, label, " + inputKey + " and expect")
		}
		seenIDs[id] = true

		mc := moldloftCase{id: id, rawJSON: rawElem}
		if json.Unmarshal(r["expect"], &mc.expectMap) != nil || mc.expectMap == nil {
			return bad("expect must be a JSON object")
		}
		switch m.Template {
		case "calculator":
			var inputsMap map[string]string
			if json.Unmarshal(r["inputs"], &inputsMap) != nil || inputsMap == nil {
				return bad("calculator worked example inputs must be an object of strings")
			}
			mc.answersMap = inputsMap
		case "checklist", "decision-tree":
			if json.Unmarshal(r["answers"], &mc.answersMap) != nil || mc.answersMap == nil {
				return bad("worked example answers must be an object of strings")
			}
		case "validator":
			if json.Unmarshal(r["input"], &mc.inputStr) != nil {
				return bad("validator worked example input must be a string")
			}
		}
		cases = append(cases, mc)
	}

	exercised := map[string]bool{}
	for _, mc := range cases {
		recordMoldloftCoverage(exercised, m, mc)
	}

	var progs []*js.Program
	progByPath := map[string]*js.Program{}
	for _, s := range m.Scripts {
		prog, err := js.Parse(s, string(b.data[s]))
		if err != nil {
			c.add(Examples, "EX-SCRIPT", EParse, s, "the logic script could not be loaded")
			return exercised, true
		}
		progs = append(progs, prog)
		progByPath[s] = prog
	}
	runnerProg, err := js.Parse("<runner>", moldloftRunnerSrc)
	if err != nil {
		c.add(Examples, "EX-SCRIPT", EParse, "tests/check.js", "the check runner could not be prepared")
		return exercised, true
	}

	if !c.verifyMoldloftDataMirrors(b, progByPath["js/data.js"], progByPath["tests/worked-examples.js"]) {
		return exercised, true
	}

	aborted := false
	for i, mc := range cases {
		if c.done() {
			aborted = true
			break
		}
		out, err := c.runMoldloftCase(progs, runnerProg, mc.rawJSON)
		interrupted := errors.Is(err, js.ErrInterrupted)
		switch {
		case interrupted:
			c.add(Examples, "EX-TIMEOUT", ETimeout, p, "the case "+mc.id+" did not finish before its deadline")
		case errors.Is(err, errLoad):
			c.add(Examples, "EX-SCRIPT", EParse, m.Scripts[0], "the logic scripts failed while loading")
		case errors.Is(err, errNoFunction):
			c.add(Examples, "EX-SCRIPT", EParse, "tests/check.js", "MoldloftCheck.run or MoldloftEngine.evaluate is not defined")
		case err != nil:
			c.add(Examples, "EX-THREW", EParse, p, "the case "+mc.id+" raised an error")
		default:
			var results []moldloftCheckResult
			if out == "" || out == "null" {
				c.add(Examples, "EX-SCRIPT", EParse, "tests/check.js", "MoldloftCheck.run or MoldloftEngine.evaluate is not defined")
				err = errNoFunction
			} else if json.Unmarshal([]byte(out), &results) != nil || len(results) != 1 || !results[0].OK || len(results[0].Problems) > 0 {
				c.add(Examples, "EX-MISMATCH", EParse, p, "the case "+mc.id+" did not give the expected result")
			}
		}
		if i == 0 && err != nil && (errors.Is(err, errLoad) || errors.Is(err, errNoFunction)) {
			aborted = true
			break
		}
		if interrupted {
			aborted = true
			break
		}
	}

	if !aborted {
		c.verifyMoldloftUI(b, m, pages, cases)
	}

	return exercised, true
}

func (c *checker) verifyMoldloftDataMirrors(b *bundle, dataProg, exProg *js.Program) bool {
	if dataProg == nil || exProg == nil {
		return false
	}
	mirrorCtx, cancel := context.WithTimeout(c.ctx, c.opts.CaseTimeout)
	defer cancel()

	vm := js.NewVM()
	if err := vm.RunProgram(mirrorCtx, dataProg); err != nil {
		if errors.Is(err, js.ErrInterrupted) {
			c.add(Examples, "EX-TIMEOUT", ETimeout, "js/data.js", "js/data.js did not finish loading before its deadline")
		} else {
			c.add(Examples, "EX-SCRIPT", EParse, "js/data.js", "js/data.js failed while loading")
		}
		return false
	}
	if err := vm.RunProgram(mirrorCtx, exProg); err != nil {
		if errors.Is(err, js.ErrInterrupted) {
			c.add(Examples, "EX-TIMEOUT", ETimeout, "tests/worked-examples.js", "tests/worked-examples.js did not finish loading before its deadline")
		} else {
			c.add(Examples, "EX-SCRIPT", EParse, "tests/worked-examples.js", "tests/worked-examples.js failed while loading")
		}
		return false
	}

	if !sameJSONValue(mirrorCtx, vm, "globalThis.MOLDLOFT_TOOL", b.data["data/tool.json"]) {
		c.add(Examples, "EX-SCRIPT", EParse, "js/data.js", "js/data.js does not match data/tool.json")
		return false
	}
	if !sameJSONValue(mirrorCtx, vm, "globalThis.MOLDLOFT_EXAMPLES", b.data["tests/worked-examples.json"]) {
		c.add(Examples, "EX-SCRIPT", EParse, "tests/worked-examples.js", "tests/worked-examples.js does not match tests/worked-examples.json")
		return false
	}
	return true
}

func sameJSONValue(ctx context.Context, vm *js.VM, expr string, diskBytes []byte) bool {
	jsOut, err := vm.EvalJSON(ctx, expr)
	if err != nil || jsOut == "" {
		return false
	}
	var fromJS, fromDisk any
	if json.Unmarshal([]byte(jsOut), &fromJS) != nil || json.Unmarshal(diskBytes, &fromDisk) != nil {
		return false
	}
	return reflect.DeepEqual(fromJS, fromDisk)
}

func recordMoldloftCoverage(exercised map[string]bool, m *manifest, mc moldloftCase) {
	if m.Tool == nil {
		return
	}
	switch m.Template {
	case "calculator":
		for stepID := range mc.expectMap {
			for _, cit := range m.Tool.StepCitations[stepID] {
				exercised[cit] = true
			}
		}
	case "checklist":
		for itemID := range mc.answersMap {
			if cit := m.Tool.ItemCitations[itemID]; cit != "" {
				exercised[cit] = true
			}
		}
	case "decision-tree":
		visited := map[string]bool{}
		cur := m.Tool.StartQuestion
		for cur != "" && !visited[cur] {
			visited[cur] = true
			if cit := m.Tool.QuestionCitations[cur]; cit != "" {
				exercised[cit] = true
			}
			ans := mc.answersMap[cur]
			opts := m.Tool.QuestionOptions[cur]
			if opts == nil {
				break
			}
			next := opts[ans]
			if cit := m.Tool.OutcomeCitations[next]; cit != "" {
				exercised[cit] = true
				break
			}
			cur = next
		}
	case "validator":
		if mc.inputStr != "" {
			for _, cits := range m.Tool.ColumnCitations {
				for _, cit := range cits {
					exercised[cit] = true
				}
			}
		}
	}
}

func (c *checker) runMoldloftCase(progs []*js.Program, runner *js.Program, caseJSON json.RawMessage) (string, error) {
	caseCtx, cancel := context.WithTimeout(c.ctx, c.opts.CaseTimeout)
	defer cancel()

	vm := js.NewVM()
	for _, prog := range progs {
		if err := vm.RunProgram(caseCtx, prog); err != nil {
			if errors.Is(err, js.ErrInterrupted) {
				return "", err
			}
			return "", errLoad
		}
	}
	if err := vm.RunProgram(caseCtx, runner); err != nil {
		return "", errLoad
	}
	return vm.CallJSON(caseCtx, "__shakedownRunMoldloftCase", caseJSON)
}

var (
	errLoad       = errors.New("load")
	errNoFunction = errors.New("no function")
)

// runCase returns the function's result as JSON text.
func (c *checker) runCase(progs []*js.Program, fn string, input json.RawMessage) (string, error) {
	caseCtx, cancel := context.WithTimeout(c.ctx, c.opts.CaseTimeout)
	defer cancel()

	vm := js.NewVM()
	for _, prog := range progs {
		if err := vm.RunProgram(caseCtx, prog); err != nil {
			if errors.Is(err, js.ErrInterrupted) {
				return "", err
			}
			return "", errLoad
		}
	}
	if !vm.HasFunction(fn) {
		return "", errNoFunction
	}
	return vm.CallJSON(caseCtx, fn, input)
}
