package check

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
)

// templates are the stack templates the suite knows.
var templates = map[string]bool{"calculator": true, "checklist": true, "validator": true, "decision-tree": true}

const (
	manifestShakedown = "shakedown"
	manifestMoldloft  = "moldloft"
)

var (
	funcRE       = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	idRE         = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	moldloftIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	sha256HexRE  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type moldloftCitation struct {
	ID    string
	Label string
	Note  string
	URL   string
}

type moldloftTool struct {
	Slug                   string
	Title                  string
	Summary                string
	ScopeLimits            []string
	Citations              []moldloftCitation
	CitationByID           map[string]moldloftCitation
	StaticLinkedCitations  map[string]bool
	InvalidCitationEntries bool
	IncompleteCitations    []string

	StepCitations     map[string][]string
	ItemCitations     map[string]string
	StartQuestion     string
	QuestionCitations map[string]string
	QuestionOptions   map[string]map[string]string
	OutcomeCitations  map[string]string
	ColumnCitations   map[string][]string
}

type manifest struct {
	Kind            string
	Contract        string
	Template        string
	Slug            string
	Entry           string
	Scripts         []string
	Function        string
	Examples        string
	Tool            *moldloftTool
	AllowedDataURLs map[string]bool
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

func (b *bundle) isMoldloft() bool {
	return !b.exists[manifestName] && b.exists[moldloftManifestName]
}

func peekMoldloftCitationURLs(b *bundle, raw []byte) map[string]bool {
	out := map[string]bool{}
	if len(raw) == 0 {
		return out
	}
	var doc struct {
		Citations []struct {
			URL string `json:"url"`
		} `json:"citations"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return out
	}
	for _, cit := range doc.Citations {
		if isAllowedCitationURL(b, cit.URL) {
			out[cit.URL] = true
		}
	}
	return out
}

func (c *checker) readManifest(b *bundle) *manifest {
	if b.exists[manifestName] && b.exists[moldloftManifestName] {
		c.add(wholeBundle, "BUNDLE-MANIFEST", EParse, moldloftManifestName, "bundle must not carry both shakedown.json and moldloft.json")
		return nil
	}
	if b.isMoldloft() {
		return c.readMoldloftManifest(b)
	}

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
	m := manifest{Kind: manifestShakedown}
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

func (c *checker) readMoldloftManifest(b *bundle) *manifest {
	bad := func(msg string) *manifest {
		c.add(wholeBundle, "BUNDLE-MANIFEST", EParse, moldloftManifestName, msg)
		return nil
	}
	data, ok := b.data[moldloftManifestName]
	if !ok {
		return nil // already reported as too large
	}
	var top map[string]json.RawMessage
	if decodeStrict(data, &top) != nil || top == nil {
		return bad("not a JSON object")
	}
	if !onlyKeys(top, "generator", "generator_version", "contract_version", "template", "item_id", "slug", "spec_sha256", "files") {
		c.add(wholeBundle, "BUNDLE-MANIFEST-FIELD", ENeedsSubtool, moldloftManifestName, "no rule covers a field in the manifest")
		return nil
	}

	str := func(raw json.RawMessage, dst *string) bool {
		return raw != nil && json.Unmarshal(raw, dst) == nil && *dst != ""
	}
	var generator, genVer, itemID, specSHA string
	m := manifest{
		Kind:            manifestMoldloft,
		AllowedDataURLs: map[string]bool{},
	}
	if !str(top["generator"], &generator) || generator != "moldloft" ||
		!str(top["generator_version"], &genVer) ||
		!str(top["contract_version"], &m.Contract) ||
		!str(top["template"], &m.Template) ||
		!str(top["item_id"], &itemID) ||
		!str(top["slug"], &m.Slug) || !idRE.MatchString(m.Slug) ||
		!str(top["spec_sha256"], &specSHA) || !sha256HexRE.MatchString(specSHA) ||
		top["files"] == nil {
		return bad("moldloft.json requires generator, generator_version, contract_version, template, item_id, slug, spec_sha256 and files")
	}
	if m.Contract != Contract {
		c.add(wholeBundle, "BUNDLE-CONTRACT", ENeedsSubtool, moldloftManifestName, "no rule covers this contract version")
		return nil
	}
	if !templates[m.Template] {
		c.add(wholeBundle, "BUNDLE-TEMPLATE", ENeedsSubtool, moldloftManifestName, "no rule covers this template")
		return nil
	}

	var rawFiles []map[string]json.RawMessage
	if json.Unmarshal(top["files"], &rawFiles) != nil || len(rawFiles) == 0 {
		return bad("files must be a non-empty list of file digests")
	}
	seen := map[string]bool{}
	for _, entry := range rawFiles {
		if !onlyKeys(entry, "path", "bytes", "sha256") {
			c.add(wholeBundle, "BUNDLE-MANIFEST-FIELD", ENeedsSubtool, moldloftManifestName, "no rule covers a field in moldloft.json files")
			return nil
		}
		var fpath, fsha string
		var fbytes int64
		if !str(entry["path"], &fpath) || !str(entry["sha256"], &fsha) || !sha256HexRE.MatchString(fsha) ||
			entry["bytes"] == nil || json.Unmarshal(entry["bytes"], &fbytes) != nil || fbytes < 0 {
			return bad("every file entry needs path, non-negative bytes and 64-char sha256")
		}
		if fpath == moldloftManifestName || seen[fpath] || !b.exists[fpath] {
			return bad("moldloft.json file list does not match the bundle contents")
		}
		seen[fpath] = true
		if fdata, loaded := b.data[fpath]; loaded {
			sum := fmt.Sprintf("%x", sha256.Sum256(fdata))
			if int64(len(fdata)) != fbytes || sum != fsha {
				return bad("file size or sha256 in moldloft.json does not match " + fpath)
			}
		}
	}
	for existing := range b.exists {
		if existing == moldloftManifestName {
			continue
		}
		if !seen[existing] {
			return bad("bundle contains unlisted file " + existing)
		}
	}

	required := []string{
		"index.html",
		"tests/index.html",
		"data/tool.json",
		"js/data.js",
		"js/engine.js",
		"js/app.js",
		"tests/worked-examples.json",
		"tests/worked-examples.js",
		"tests/check.js",
	}
	if m.Template == "calculator" {
		required = append(required, "js/decimal.js")
	}
	for _, req := range required {
		if !b.exists[req] {
			return bad("moldloft bundle is missing required file " + req)
		}
	}

	m.Entry = "index.html"
	m.Examples = "tests/worked-examples.json"
	if m.Template == "calculator" {
		m.Scripts = []string{"js/data.js", "js/decimal.js", "js/engine.js", "tests/worked-examples.js", "tests/check.js"}
	} else {
		m.Scripts = []string{"js/data.js", "js/engine.js", "tests/worked-examples.js", "tests/check.js"}
	}

	tool := c.readMoldloftTool(b, &m)
	if tool == nil {
		return nil
	}
	m.Tool = tool
	return &m
}

func (c *checker) readMoldloftTool(b *bundle, m *manifest) *moldloftTool {
	const toolPath = "data/tool.json"
	raw, ok := b.data[toolPath]
	if !ok {
		return nil
	}
	var top map[string]json.RawMessage
	if decodeStrict(raw, &top) != nil || top == nil {
		return nil // checkJSONFiles reports BUNDLE-JSON
	}

	allowed := []string{"contract_version", "template", "slug", "title", "summary", "scope_limits", "citations"}
	switch m.Template {
	case "calculator":
		allowed = append(allowed, "inputs", "constants", "tables", "steps", "outputs")
	case "checklist":
		allowed = append(allowed, "items")
	case "decision-tree":
		allowed = append(allowed, "start", "questions", "outcomes")
	case "validator":
		allowed = append(allowed, "delimiter", "separator", "has_header", "tables", "columns")
	}
	if !onlyKeys(top, allowed...) {
		c.add(wholeBundle, "BUNDLE-MANIFEST-FIELD", ENeedsSubtool, toolPath, "no rule covers a field in data/tool.json")
		return nil
	}

	bad := func(msg string) *moldloftTool {
		c.add(wholeBundle, "BUNDLE-MANIFEST", EParse, toolPath, msg)
		return nil
	}
	if top["contract_version"] != nil {
		var cv string
		if json.Unmarshal(top["contract_version"], &cv) != nil || cv == "" {
			return bad("contract_version in data/tool.json must be a string")
		}
		if cv != Contract {
			c.add(wholeBundle, "BUNDLE-CONTRACT", ENeedsSubtool, toolPath, "no rule covers this contract version")
			return nil
		}
	}
	if top["template"] != nil {
		var tmpl string
		if json.Unmarshal(top["template"], &tmpl) != nil || tmpl != m.Template {
			return bad("template in data/tool.json does not match moldloft.json")
		}
	}

	t := &moldloftTool{
		CitationByID:          map[string]moldloftCitation{},
		StaticLinkedCitations: map[string]bool{},
		StepCitations:         map[string][]string{},
		ItemCitations:         map[string]string{},
		QuestionCitations:     map[string]string{},
		QuestionOptions:       map[string]map[string]string{},
		OutcomeCitations:      map[string]string{},
		ColumnCitations:       map[string][]string{},
	}
	str := func(raw json.RawMessage, dst *string) bool {
		return raw != nil && json.Unmarshal(raw, dst) == nil && strings.TrimSpace(*dst) != ""
	}
	if !str(top["slug"], &t.Slug) || t.Slug != m.Slug || !str(top["title"], &t.Title) || !str(top["summary"], &t.Summary) {
		return bad("data/tool.json requires matching slug, title and summary")
	}
	if top["scope_limits"] == nil || json.Unmarshal(top["scope_limits"], &t.ScopeLimits) != nil || len(t.ScopeLimits) == 0 {
		return bad("data/tool.json requires a non-empty scope_limits list")
	}
	for _, lim := range t.ScopeLimits {
		if strings.TrimSpace(lim) == "" {
			return bad("scope_limits entries must not be empty")
		}
	}

	var rawCitations []map[string]json.RawMessage
	if top["citations"] == nil || json.Unmarshal(top["citations"], &rawCitations) != nil {
		return bad("data/tool.json requires a citations list")
	}
	for _, rc := range rawCitations {
		if !onlyKeys(rc, "id", "label", "note", "url") {
			c.add(wholeBundle, "BUNDLE-MANIFEST-FIELD", ENeedsSubtool, toolPath, "no rule covers a citation field in data/tool.json")
			return nil
		}
		var cit moldloftCitation
		okID := rc["id"] != nil && json.Unmarshal(rc["id"], &cit.ID) == nil && moldloftIDRE.MatchString(cit.ID)
		okLabel := rc["label"] != nil && json.Unmarshal(rc["label"], &cit.Label) == nil && strings.TrimSpace(cit.Label) != ""
		if rc["note"] != nil && json.Unmarshal(rc["note"], &cit.Note) != nil {
			t.InvalidCitationEntries = true
			continue
		}
		if rc["url"] != nil {
			if json.Unmarshal(rc["url"], &cit.URL) != nil || !isAllowedCitationURL(b, cit.URL) {
				t.InvalidCitationEntries = true
				continue
			}
			m.AllowedDataURLs[cit.URL] = true
		}
		if !okID || !okLabel {
			t.InvalidCitationEntries = true
			continue
		}
		if _, dup := t.CitationByID[cit.ID]; dup {
			t.InvalidCitationEntries = true
			continue
		}
		t.Citations = append(t.Citations, cit)
		t.CitationByID[cit.ID] = cit
	}

	checkCitRef := func(ref, owner string, required bool, staticLinked bool) bool {
		if ref == "" {
			if required {
				t.IncompleteCitations = append(t.IncompleteCitations, owner+" has no citation")
			}
			return false
		}
		if _, ok := t.CitationByID[ref]; !ok {
			t.IncompleteCitations = append(t.IncompleteCitations, owner+" cites unknown source "+ref)
			return false
		}
		if staticLinked {
			t.StaticLinkedCitations[ref] = true
		}
		return true
	}

	switch m.Template {
	case "calculator":
		var inputs []struct {
			ID    string `json:"id"`
			Table string `json:"table"`
		}
		var constants []struct {
			ID       string `json:"id"`
			Citation string `json:"citation"`
		}
		var tables []struct {
			ID       string `json:"id"`
			Citation string `json:"citation"`
		}
		var steps []struct {
			ID       string   `json:"id"`
			Args     []string `json:"args"`
			Citation string   `json:"citation"`
		}
		var outputs []string
		if top["inputs"] == nil || json.Unmarshal(top["inputs"], &inputs) != nil ||
			top["steps"] == nil || json.Unmarshal(top["steps"], &steps) != nil || len(steps) == 0 ||
			top["outputs"] == nil || json.Unmarshal(top["outputs"], &outputs) != nil || len(outputs) == 0 {
			return bad("calculator tool.json requires inputs, steps and outputs")
		}
		if top["constants"] != nil && json.Unmarshal(top["constants"], &constants) != nil {
			return bad("calculator constants must be a list")
		}
		if top["tables"] != nil && json.Unmarshal(top["tables"], &tables) != nil {
			return bad("calculator tables must be a list")
		}
		constCit := map[string]string{}
		for _, k := range constants {
			if checkCitRef(k.Citation, "constant "+k.ID, false, true) {
				constCit[k.ID] = k.Citation
			}
		}
		tableCit := map[string]string{}
		for _, tb := range tables {
			if checkCitRef(tb.Citation, "table "+tb.ID, false, true) {
				tableCit[tb.ID] = tb.Citation
			}
		}
		inputTable := map[string]string{}
		for _, in := range inputs {
			if in.Table != "" {
				inputTable[in.ID] = in.Table
			}
		}
		for _, st := range steps {
			if !moldloftIDRE.MatchString(st.ID) {
				return bad("calculator step id is invalid")
			}
			var cits []string
			if checkCitRef(st.Citation, "step "+st.ID, false, true) {
				cits = append(cits, st.Citation)
			}
			for _, arg := range st.Args {
				if cc := constCit[arg]; cc != "" {
					cits = append(cits, cc)
				}
				if tc := tableCit[arg]; tc != "" {
					cits = append(cits, tc)
				}
				if tbl := inputTable[arg]; tbl != "" && tableCit[tbl] != "" {
					cits = append(cits, tableCit[tbl])
				}
			}
			t.StepCitations[st.ID] = cits
		}

	case "checklist":
		var items []struct {
			ID       string `json:"id"`
			Label    string `json:"label"`
			Kind     string `json:"kind"`
			Citation string `json:"citation"`
		}
		if top["items"] == nil || json.Unmarshal(top["items"], &items) != nil || len(items) == 0 {
			return bad("checklist tool.json requires a non-empty items list")
		}
		for _, it := range items {
			if !moldloftIDRE.MatchString(it.ID) || strings.TrimSpace(it.Label) == "" || it.Kind == "" {
				return bad("every checklist item needs id, label and kind")
			}
			if checkCitRef(it.Citation, "item "+it.ID, true, true) {
				t.ItemCitations[it.ID] = it.Citation
			}
		}

	case "decision-tree":
		var questions []struct {
			ID       string `json:"id"`
			Prompt   string `json:"prompt"`
			Citation string `json:"citation"`
			Options  []struct {
				Key   string `json:"key"`
				Label string `json:"label"`
				Next  string `json:"next"`
			} `json:"options"`
		}
		var outcomes []struct {
			ID       string `json:"id"`
			Title    string `json:"title"`
			Summary  string `json:"summary"`
			Status   string `json:"status"`
			Citation string `json:"citation"`
		}
		if !str(top["start"], &t.StartQuestion) ||
			top["questions"] == nil || json.Unmarshal(top["questions"], &questions) != nil || len(questions) == 0 ||
			top["outcomes"] == nil || json.Unmarshal(top["outcomes"], &outcomes) != nil || len(outcomes) == 0 {
			return bad("decision-tree tool.json requires start, questions and outcomes")
		}
		for _, q := range questions {
			if !moldloftIDRE.MatchString(q.ID) || strings.TrimSpace(q.Prompt) == "" || len(q.Options) == 0 {
				return bad("every decision-tree question needs id, prompt and options")
			}
			if checkCitRef(q.Citation, "question "+q.ID, true, true) {
				t.QuestionCitations[q.ID] = q.Citation
			}
			opts := map[string]string{}
			for _, op := range q.Options {
				if op.Key == "" || op.Next == "" {
					return bad("every decision-tree option needs key and next")
				}
				opts[op.Key] = op.Next
			}
			t.QuestionOptions[q.ID] = opts
		}
		for _, out := range outcomes {
			if !moldloftIDRE.MatchString(out.ID) || strings.TrimSpace(out.Title) == "" || out.Status == "" {
				return bad("every decision-tree outcome needs id, title and status")
			}
			if checkCitRef(out.Citation, "outcome "+out.ID, true, false) {
				t.OutcomeCitations[out.ID] = out.Citation
			}
		}

	case "validator":
		var delim, sep string
		var tables []struct {
			ID       string   `json:"id"`
			Citation string   `json:"citation"`
			Codes    []string `json:"codes"`
		}
		var columns []struct {
			ID       string `json:"id"`
			Label    string `json:"label"`
			Rule     string `json:"rule"`
			Citation string `json:"citation"`
			Table    string `json:"table"`
		}
		if !str(top["delimiter"], &delim) || top["separator"] == nil || json.Unmarshal(top["separator"], &sep) != nil || sep == "" ||
			top["columns"] == nil || json.Unmarshal(top["columns"], &columns) != nil || len(columns) == 0 {
			return bad("validator tool.json requires delimiter, separator and columns")
		}
		if top["tables"] != nil && json.Unmarshal(top["tables"], &tables) != nil {
			return bad("validator tables must be a list")
		}
		tableCit := map[string]string{}
		for _, tb := range tables {
			if checkCitRef(tb.Citation, "table "+tb.ID, false, true) {
				tableCit[tb.ID] = tb.Citation
			}
		}
		for _, col := range columns {
			if !moldloftIDRE.MatchString(col.ID) || strings.TrimSpace(col.Label) == "" || col.Rule == "" {
				return bad("every validator column needs id, label and rule")
			}
			var cits []string
			if checkCitRef(col.Citation, "column "+col.ID, true, true) {
				cits = append(cits, col.Citation)
			}
			if col.Table != "" && tableCit[col.Table] != "" {
				cits = append(cits, tableCit[col.Table])
			}
			t.ColumnCitations[col.ID] = cits
		}
	}

	return t
}

func isAllowedCitationURL(b *bundle, raw string) bool {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return false
	}
	lower := strings.ToLower(raw)
	if !strings.HasPrefix(lower, "https://") && !strings.HasPrefix(lower, "http://") {
		return false
	}
	return b.classifyRef("data/tool.json", raw) == refRemote
}

// checkJSONFiles makes sure every bundled data file is whole.
func (c *checker) checkJSONFiles(b *bundle) {
	for _, p := range b.withExt(".json") {
		if p == manifestName || p == moldloftManifestName {
			continue
		}
		var v any
		if decodeStrict(b.data[p], &v) != nil {
			c.add(wholeBundle, "BUNDLE-JSON", EParse, p, "not valid JSON")
		}
	}
}
