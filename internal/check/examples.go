package check

import (
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/dop251/goja"
)

const examplesFormat = "v1"

var exampleKinds = map[string]bool{"worked": true, "boundary": true, "synthetic": true}

// fixedClock is what a tool's script sees as "now". Examples that depend on
// a date must pass it in as input.
var fixedClock = time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)

func init() {
	// The script engine reads the process time zone for local-time methods.
	// Pin it so the same input gives the same output on any machine.
	time.Local = time.UTC
}

// prelude removes the one source of randomness a script can reach.
const prelude = `Math.random = function () { throw new TypeError("Math.random is not available"); };`

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

	var progs []*goja.Program
	for _, s := range m.Scripts {
		prog, err := goja.Compile(s, string(b.data[s]), false)
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
		var interrupted *goja.InterruptedError
		switch {
		case errors.As(err, &interrupted):
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
		if interrupted != nil {
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
func (c *checker) runCase(progs []*goja.Program, fn string, input json.RawMessage) (string, error) {
	vm := goja.New()
	vm.SetTimeSource(func() time.Time { return fixedClock })
	vm.SetRandSource(func() float64 { return 0 })
	vm.SetMaxCallStackSize(512)
	timer := time.AfterFunc(c.opts.CaseTimeout, func() { vm.Interrupt("deadline") })
	defer timer.Stop()

	var interrupted *goja.InterruptedError
	if _, err := vm.RunString(prelude); err != nil {
		return "", err
	}
	for _, prog := range progs {
		if _, err := vm.RunProgram(prog); err != nil {
			if errors.As(err, &interrupted) {
				return "", err
			}
			return "", errLoad
		}
	}
	kind, err := vm.RunString("typeof " + fn)
	if err != nil {
		return "", err
	}
	if kind.String() != "function" {
		return "", errNoFunction
	}
	literal, err := json.Marshal(string(input))
	if err != nil {
		return "", err
	}
	v, err := vm.RunString("JSON.stringify(" + fn + "(JSON.parse(" + string(literal) + ")))")
	if err != nil {
		return "", err
	}
	if goja.IsUndefined(v) {
		return "", nil
	}
	return v.String(), nil
}
