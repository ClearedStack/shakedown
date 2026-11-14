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

// runExamples returns the calculations the examples exercise, and whether
// the examples file could be read at all.
func (c *checker) runExamples(b *bundle, m *manifest) (map[string]bool, bool) {
	if m == nil {
		return nil, false
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
