package check

import (
	"bytes"
	"encoding/json"
	"io"
	"path"
	"regexp"
)

// templates are the stack templates the suite knows.
var templates = map[string]bool{"calculator": true, "checklist": true, "validator": true, "decision-tree": true}

var (
	funcRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	idRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
)

type manifest struct {
	Contract string
	Template string
	Entry    string
	Scripts  []string
	Function string
	Examples string
}

// decodeStrict decodes exactly one JSON value and nothing after it.
func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return io.ErrUnexpectedEOF
	}
	return nil
}

// onlyKeys reports whether obj has no key outside allowed.
func onlyKeys(obj map[string]json.RawMessage, allowed ...string) bool {
	ok := map[string]bool{}
	for _, k := range allowed {
		ok[k] = true
	}
	for k := range obj {
		if !ok[k] {
			return false
		}
	}
	return true
}

func (c *checker) readManifest(b *bundle) *manifest {
	bad := func(msg string) *manifest {
		c.add(wholeBundle, "BUNDLE-MANIFEST", EParse, manifestName, msg)
		return nil
	}
	data, ok := b.data[manifestName]
	if !ok {
		if b.exists[manifestName] {
			return nil // already reported as too large
		}
		c.add(wholeBundle, "BUNDLE-MANIFEST", EParse, "", "the bundle has no "+manifestName)
		return nil
	}
	var top map[string]json.RawMessage
	if decodeStrict(data, &top) != nil || top == nil {
		return bad("not a JSON object")
	}
	if !onlyKeys(top, "contract", "template", "entry", "logic", "examples") {
		c.add(wholeBundle, "BUNDLE-MANIFEST-FIELD", ENeedsSubtool, manifestName, "no rule covers a field in the manifest")
		return nil
	}
	var m manifest
	var logic map[string]json.RawMessage
	str := func(raw json.RawMessage, dst *string) bool {
		return raw != nil && json.Unmarshal(raw, dst) == nil && *dst != ""
	}
	if !str(top["contract"], &m.Contract) || !str(top["template"], &m.Template) ||
		!str(top["entry"], &m.Entry) || !str(top["examples"], &m.Examples) ||
		top["logic"] == nil || json.Unmarshal(top["logic"], &logic) != nil || logic == nil {
		return bad("contract, template, entry, logic and examples are all required")
	}
	if !onlyKeys(logic, "scripts", "function") {
		c.add(wholeBundle, "BUNDLE-MANIFEST-FIELD", ENeedsSubtool, manifestName, "no rule covers a field in the manifest")
		return nil
	}
	if logic["scripts"] == nil || json.Unmarshal(logic["scripts"], &m.Scripts) != nil || len(m.Scripts) == 0 ||
		!str(logic["function"], &m.Function) || !funcRE.MatchString(m.Function) {
		return bad("logic needs a list of scripts and a plain function name")
	}
	if m.Contract != Contract {
		c.add(wholeBundle, "BUNDLE-CONTRACT", ENeedsSubtool, manifestName, "no rule covers this contract version")
		return nil
	}
	if !templates[m.Template] {
		c.add(wholeBundle, "BUNDLE-TEMPLATE", ENeedsSubtool, manifestName, "no rule covers this template")
		return nil
	}
	if path.Ext(m.Entry) != ".html" || !b.exists[m.Entry] {
		return bad("entry must name an .html file in the bundle")
	}
	for _, s := range m.Scripts {
		if path.Ext(s) != ".js" || !b.exists[s] {
			return bad("every logic script must name a .js file in the bundle")
		}
	}
	if path.Ext(m.Examples) != ".json" || !b.exists[m.Examples] {
		return bad("examples must name a .json file in the bundle")
	}
	return &m
}

// checkJSONFiles makes sure every bundled data file is whole.
func (c *checker) checkJSONFiles(b *bundle) {
	for _, p := range b.withExt(".json") {
		if p == manifestName {
			continue
		}
		var v any
		if decodeStrict(b.data[p], &v) != nil {
			c.add(wholeBundle, "BUNDLE-JSON", EParse, p, "not valid JSON")
		}
	}
}
