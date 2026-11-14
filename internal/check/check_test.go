package check

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the saved expected outputs")

const testdata = "../../testdata"

// golden compares got with the saved file, or rewrites it under -update.
func golden(t *testing.T, file string, got []byte) {
	t.Helper()
	if *update {
		if err := os.WriteFile(file, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("no saved output: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("output differs from %s\n--- got ---\n%s--- want ---\n%s", file, got, want)
	}
}

func render(t *testing.T, r Report) (text, js []byte) {
	t.Helper()
	var a, b bytes.Buffer
	if err := r.WriteText(&a); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteJSON(&b); err != nil {
		t.Fatal(err)
	}
	return a.Bytes(), b.Bytes()
}

// The two sample tools named by the first milestone: one that passes every
// condition and one that fails every condition.
func TestSampleTools(t *testing.T) {
	for name, want := range map[string]Status{"passing-tool": Pass, "failing-tool": Fail} {
		t.Run(name, func(t *testing.T) {
			r, err := Dir(context.Background(), filepath.Join(testdata, "tools", name), Options{})
			if err != nil {
				t.Fatal(err)
			}
			if r.Verdict != want {
				t.Errorf("verdict %s, want %s", r.Verdict, want)
			}
			for _, c := range r.Conditions {
				if c.Status != want {
					t.Errorf("%s is %s, want %s", c.Condition, c.Status, want)
				}
			}
			text, js := render(t, r)
			golden(t, filepath.Join(testdata, "expected", name+".txt"), text)
			golden(t, filepath.Join(testdata, "expected", name+".json"), js)
		})
	}
}

type fixtureOptions struct {
	Expired       bool `json:"expired"`
	CaseTimeoutMs int  `json:"caseTimeoutMs"`
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(dst, 0o755)
		case d.Type()&os.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			os.Remove(dst)
			return os.Symlink(target, dst)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Every rule has a saved example: the passing sample tool with one change
// laid over it, and the exact report that change must produce.
func TestRules(t *testing.T) {
	dirs, err := os.ReadDir(filepath.Join(testdata, "rules"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		rule := d.Name()
		t.Run(rule, func(t *testing.T) {
			fixture := filepath.Join(testdata, "rules", rule)
			work := t.TempDir()
			copyTree(t, filepath.Join(testdata, "tools", "passing-tool"), work)
			if _, err := os.Stat(filepath.Join(fixture, "files")); err == nil {
				copyTree(t, filepath.Join(fixture, "files"), work)
			}
			if list, err := os.ReadFile(filepath.Join(fixture, "remove.txt")); err == nil {
				for _, name := range strings.Fields(string(list)) {
					if err := os.Remove(filepath.Join(work, name)); err != nil {
						t.Fatal(err)
					}
				}
			}
			var fo fixtureOptions
			if data, err := os.ReadFile(filepath.Join(fixture, "options.json")); err == nil {
				if err := json.Unmarshal(data, &fo); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if fo.Expired {
				cancel()
			}
			r, err := Dir(ctx, work, Options{CaseTimeout: time.Duration(fo.CaseTimeoutMs) * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, f := range r.Findings {
				found = found || f.Rule == rule
			}
			if !found {
				t.Errorf("the fixture did not trigger %s", rule)
			}
			if r.Verdict == Pass {
				t.Errorf("the fixture passed")
			}
			text, _ := render(t, r)
			if strings.Contains(string(text), work) || strings.Contains(string(text), os.TempDir()) {
				t.Errorf("the report names a place on this machine:\n%s", text)
			}
			golden(t, filepath.Join(fixture, "expected.txt"), text)
		})
	}
}

var ruleRE = regexp.MustCompile(`"((?:BUNDLE|OFF|CSP|TRK|EX|CIT)-[A-Z-]+)"`)

// A rule added to the code without a saved example fails here, and so does
// a saved example for a rule that no longer exists.
func TestEveryRuleHasAFixture(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	inCode := map[string]bool{}
	for _, s := range sources {
		if strings.HasSuffix(s, "_test.go") {
			continue
		}
		data, err := os.ReadFile(s)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range ruleRE.FindAllSubmatch(data, -1) {
			inCode[string(m[1])] = true
		}
	}
	if len(inCode) == 0 {
		t.Fatal("found no rules in the source")
	}
	dirs, err := os.ReadDir(filepath.Join(testdata, "rules"))
	if err != nil {
		t.Fatal(err)
	}
	saved := map[string]bool{}
	for _, d := range dirs {
		saved[d.Name()] = true
		if !inCode[d.Name()] {
			t.Errorf("saved example %s matches no rule in the code", d.Name())
		}
	}
	for _, rule := range sortedKeys(inCode) {
		if !saved[rule] {
			t.Errorf("rule %s has no saved example", rule)
		}
	}
}

// The same input gives the same output, however many times it runs.
func TestSameInputSameOutput(t *testing.T) {
	var first []byte
	for i := 0; i < 5; i++ {
		r, err := Dir(context.Background(), filepath.Join(testdata, "tools", "failing-tool"), Options{})
		if err != nil {
			t.Fatal(err)
		}
		_, js := render(t, r)
		if first == nil {
			first = js
		} else if !bytes.Equal(first, js) {
			t.Fatal("two runs over the same folder gave different reports")
		}
	}
}

// The names in the code are the names in the registry entry.
func TestNamesMatchRegistry(t *testing.T) {
	data, err := os.ReadFile("../../component.json")
	if err != nil {
		t.Fatal(err)
	}
	var entry struct {
		Slug     string   `json:"slug"`
		Version  string   `json:"version"`
		SubTools []string `json:"subTools"`
	}
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Slug != Component || entry.Version != Version {
		t.Errorf("code says %s %s, component.json says %s %s", Component, Version, entry.Slug, entry.Version)
	}
	if len(entry.SubTools) != 0 {
		t.Errorf("component.json lists sub-tools; this build has none")
	}
}

func TestOnlyThreePublicCodes(t *testing.T) {
	allowed := map[Code]bool{EParse: true, ETimeout: true, ENeedsSubtool: true}
	files, _ := filepath.Glob(filepath.Join(testdata, "rules", "*", "expected.txt"))
	if len(files) == 0 {
		t.Fatal("no saved outputs")
	}
	codeRE := regexp.MustCompile(`\bE_[A-Z_]+\b`)
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range codeRE.FindAll(data, -1) {
			if !allowed[Code(m)] {
				t.Errorf("%s uses the code %s", f, m)
			}
		}
	}
}

func TestUnreadableFolder(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-here")
	_, err := Dir(context.Background(), missing, Options{})
	if err != ErrUnreadable {
		t.Fatalf("got %v, want ErrUnreadable", err)
	}
	if strings.Contains(err.Error(), missing) {
		t.Errorf("the error names the folder")
	}
}

func TestClassifyRef(t *testing.T) {
	b := &bundle{exists: map[string]bool{"index.html": true, "img/logo.png": true, "docs/help.html": true}}
	cases := []struct {
		from, raw string
		want      refKind
	}{
		{"index.html", "", refEmpty},
		{"index.html", "#top", refFragment},
		{"index.html", "img/logo.png", refLocal},
		{"index.html", "./img/logo.png?v=2#x", refLocal},
		{"docs/help.html", "../img/logo.png", refLocal},
		{"docs/help.html", "img/logo.png", refMissing},
		{"index.html", "../secret.txt", refOutside},
		{"docs/help.html", "../../secret.txt", refOutside},
		{"index.html", "https://images.example/a.png", refRemote},
		{"index.html", "HTTP://images.example/a.png", refRemote},
		{"index.html", "//images.example/a.png", refRemote},
		{"index.html", "wss://images.example/feed", refRemote},
		{"index.html", " javascript:start()", refInlineCode},
		{"index.html", "data:text/plain,hello", refUnfamiliar},
		{"index.html", "mailto:someone@mail.example", refUnfamiliar},
		{"index.html", "/img/logo.png", refUnfamiliar},
		{"index.html", "img/logo%2epng", refUnfamiliar},
		{"index.html", `\\images.example\a.png`, refUnfamiliar},
		{"index.html", "ht\ntps://images.example/a.png", refUnfamiliar},
	}
	for _, c := range cases {
		if got := b.classifyRef(c.from, c.raw); got != c.want {
			t.Errorf("classifyRef(%q, %q) = %d, want %d", c.from, c.raw, got, c.want)
		}
	}
}

func TestStatusOrder(t *testing.T) {
	r := buildReport([]Finding{
		{Condition: CSP, Rule: "CSP-UNFAMILIAR", Code: ENeedsSubtool, Message: "x"},
		{Condition: Examples, Rule: "EX-TIMEOUT", Code: ETimeout, Message: "x"},
		{Condition: Examples, Rule: "EX-MISMATCH", Code: EParse, Message: "x"},
		{Condition: Examples, Rule: "EX-MISMATCH", Code: EParse, Message: "x"},
	})
	got := map[string]Status{}
	for _, c := range r.Conditions {
		got[c.Condition] = c.Status
	}
	want := map[string]Status{Offline: Pass, CSP: NeedsSubtool, Tracking: Pass, Examples: Fail, Citations: Pass}
	for _, c := range conditionOrder {
		if got[c] != want[c] {
			t.Errorf("%s is %s, want %s", c, got[c], want[c])
		}
	}
	if r.Verdict != Fail {
		t.Errorf("verdict %s, want fail", r.Verdict)
	}
	if len(r.Findings) != 3 {
		t.Errorf("%d findings, want 3 after removing the repeat", len(r.Findings))
	}
	if !sort.SliceIsSorted(r.Findings, func(i, j int) bool {
		return conditionIndex(r.Findings[i].Condition) < conditionIndex(r.Findings[j].Condition)
	}) {
		t.Errorf("findings are not in condition order")
	}
}

func TestMapMatchesDirAndPathGuards(t *testing.T) {
	for _, name := range []string{"passing-tool", "failing-tool"} {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(testdata, "tools", name)
			files := map[string][]byte{}
			err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				rel, err := filepath.Rel(root, p)
				if err != nil {
					return err
				}
				data, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				files[filepath.ToSlash(rel)] = data
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			dirRep, err := Dir(context.Background(), root, Options{})
			if err != nil {
				t.Fatal(err)
			}
			mapRep := Map(context.Background(), files, Options{})
			_, dirJSON := render(t, dirRep)
			_, mapJSON := render(t, mapRep)
			if !bytes.Equal(dirJSON, mapJSON) {
				t.Errorf("Map output differs from Dir output:\n--- Map ---\n%s\n--- Dir ---\n%s", mapJSON, dirJSON)
			}
		})
	}

	t.Run("rejects traversal and leading slash", func(t *testing.T) {
		r := Map(context.Background(), map[string][]byte{
			"../secret.txt": []byte("x"),
			"/abs.txt":      []byte("x"),
		}, Options{})
		if r.Verdict != Fail {
			t.Fatalf("verdict %s, want fail", r.Verdict)
		}
		found := false
		for _, f := range r.Findings {
			found = found || f.Rule == "BUNDLE-FILE-NAME"
		}
		if !found {
			t.Fatalf("expected BUNDLE-FILE-NAME in %+v", r.Findings)
		}
	})
}

func loadDirMap(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = data
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func recomputeMoldloftManifest(t *testing.T, files map[string][]byte) {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(files["moldloft.json"], &doc); err != nil {
		t.Fatal(err)
	}
	var names []string
	for name := range files {
		if name == "moldloft.json" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	entries := make([]map[string]any, 0, len(names))
	for _, name := range names {
		sum := sha256.Sum256(files[name])
		entries = append(entries, map[string]any{
			"path":   name,
			"bytes":  len(files[name]),
			"sha256": hex.EncodeToString(sum[:]),
		})
	}
	doc["files"] = entries
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	files["moldloft.json"] = append(raw, '\n')
}

func assertHasRule(t *testing.T, r Report, wantVerdict Status, wantRule string) {
	t.Helper()
	if r.Verdict != wantVerdict {
		t.Fatalf("verdict = %s, want %s (findings: %+v)", r.Verdict, wantVerdict, r.Findings)
	}
	for _, f := range r.Findings {
		if f.Rule == wantRule {
			return
		}
	}
	t.Fatalf("missing rule %s in findings: %+v", wantRule, r.Findings)
}

func TestMoldloftGoldenBundles(t *testing.T) {
	dirs, err := os.ReadDir(filepath.Join(testdata, "moldloft"))
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) < 5 {
		t.Fatalf("expected at least 5 Moldloft golden fixtures, got %d", len(dirs))
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		name := d.Name()
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(testdata, "moldloft", name)
			dirRep, err := Dir(context.Background(), root, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if dirRep.Verdict != Pass {
				t.Fatalf("verdict = %s, want pass; findings: %+v", dirRep.Verdict, dirRep.Findings)
			}
			for _, c := range dirRep.Conditions {
				if c.Status != Pass {
					t.Errorf("condition %s = %s, want pass", c.Condition, c.Status)
				}
			}
			mapRep := Map(context.Background(), loadDirMap(t, root), Options{})
			_, dirJSON := render(t, dirRep)
			_, mapJSON := render(t, mapRep)
			if !bytes.Equal(dirJSON, mapJSON) {
				t.Errorf("Map output differs from Dir output:\n--- Map ---\n%s\n--- Dir ---\n%s", mapJSON, dirJSON)
			}
		})
	}
}

func TestMoldloftTamperAndMarkdownChecks(t *testing.T) {
	t.Run("manifest_digest_mismatch", func(t *testing.T) {
		files := loadDirMap(t, filepath.Join(testdata, "moldloft", "calculator-rate-buildup"))
		files["README.md"] = append(files["README.md"], []byte("\nextra line\n")...)
		r := Map(context.Background(), files, Options{})
		assertHasRule(t, r, Fail, "BUNDLE-MANIFEST")
	})

	t.Run("manifest_unlisted_file", func(t *testing.T) {
		files := loadDirMap(t, filepath.Join(testdata, "moldloft", "checklist-solicitation-review"))
		files["docs/extra.md"] = []byte("# Extra\n")
		r := Map(context.Background(), files, Options{})
		assertHasRule(t, r, Fail, "BUNDLE-MANIFEST")
	})

	t.Run("unsupported_template_pauses_needs_subtool", func(t *testing.T) {
		files := loadDirMap(t, filepath.Join(testdata, "moldloft", "checklist-solicitation-review"))
		files["moldloft.json"] = bytes.Replace(files["moldloft.json"], []byte(`"checklist"`), []byte(`"spreadsheet"`), 1)
		r := Map(context.Background(), files, Options{})
		assertHasRule(t, r, NeedsSubtool, "BUNDLE-TEMPLATE")
	})

	t.Run("data_js_mirror_divergence", func(t *testing.T) {
		files := loadDirMap(t, filepath.Join(testdata, "moldloft", "calculator-rate-buildup"))
		files["js/data.js"] = bytes.Replace(files["js/data.js"], []byte(`"Sample hourly rate build-up"`), []byte(`"Tampered title"`), 1)
		recomputeMoldloftManifest(t, files)
		r := Map(context.Background(), files, Options{})
		assertHasRule(t, r, Fail, "EX-SCRIPT")
	})

	t.Run("worked_examples_js_mirror_divergence", func(t *testing.T) {
		files := loadDirMap(t, filepath.Join(testdata, "moldloft", "validator-line-code-check"))
		files["tests/worked-examples.js"] = bytes.Replace(files["tests/worked-examples.js"], []byte(`"Two valid schedule rows"`), []byte(`"Different label"`), 1)
		recomputeMoldloftManifest(t, files)
		r := Map(context.Background(), files, Options{})
		assertHasRule(t, r, Fail, "EX-SCRIPT")
	})

	t.Run("worked_example_expected_mismatch_all_four_templates", func(t *testing.T) {
		cases := []struct {
			bundle string
			old    string
			bad    string
		}{
			{"calculator-rate-buildup", `"6737.00"`, `"6737.01"`},
			{"checklist-solicitation-review", `"unmet_required": 0`, `"unmet_required": 2`},
			{"decision-tree-clause-routing", `"micro_purchase_ok"`, `"specialist_review"`},
			{"validator-line-code-check", `"error_count": 2`, `"error_count": 1`},
		}
		for _, tc := range cases {
			t.Run(tc.bundle, func(t *testing.T) {
				files := loadDirMap(t, filepath.Join(testdata, "moldloft", tc.bundle))
				files["tests/worked-examples.json"] = bytes.Replace(files["tests/worked-examples.json"], []byte(tc.old), []byte(tc.bad), 1)
				files["tests/worked-examples.js"] = bytes.Replace(files["tests/worked-examples.js"], []byte(tc.old), []byte(tc.bad), 1)
				recomputeMoldloftManifest(t, files)
				r := Map(context.Background(), files, Options{})
				assertHasRule(t, r, Fail, "EX-MISMATCH")
			})
		}
	})

	t.Run("remote_url_in_engine_js_rejected_even_if_citation_url", func(t *testing.T) {
		files := loadDirMap(t, filepath.Join(testdata, "moldloft", "calculator-rate-buildup"))
		files["js/engine.js"] = append(files["js/engine.js"], []byte("\nconst leaked = \"https://rules.example/sample-rule-12-3\";\n")...)
		recomputeMoldloftManifest(t, files)
		r := Map(context.Background(), files, Options{})
		assertHasRule(t, r, Fail, "OFF-REMOTE-STRING")
	})

	t.Run("unlisted_remote_url_in_data_js_rejected", func(t *testing.T) {
		files := loadDirMap(t, filepath.Join(testdata, "moldloft", "calculator-rate-buildup"))
		files["data/tool.json"] = bytes.Replace(files["data/tool.json"], []byte(`"Sample hourly rate build-up"`), []byte(`"https://evil.example/beacon"`), 1)
		files["js/data.js"] = bytes.Replace(files["js/data.js"], []byte(`"Sample hourly rate build-up"`), []byte(`"https://evil.example/beacon"`), 1)
		recomputeMoldloftManifest(t, files)
		r := Map(context.Background(), files, Options{})
		assertHasRule(t, r, Fail, "OFF-REMOTE-STRING")
	})

	t.Run("markdown_broken_link_and_remote_image", func(t *testing.T) {
		files := loadDirMap(t, filepath.Join(testdata, "moldloft", "checklist-solicitation-review"))
		mdExtra := "\n```markdown\n[ignored](does-not-exist.md)\n```\n\nSee [missing](missing.md) and ![beacon](https://evil.example/p.png).\n"
		files["README.md"] = append(files["README.md"], []byte(mdExtra)...)
		recomputeMoldloftManifest(t, files)
		r := Map(context.Background(), files, Options{})
		assertHasRule(t, r, Fail, "OFF-MISSING-REFERENCE")
		assertHasRule(t, r, Fail, "OFF-REMOTE-REFERENCE")
	})

	t.Run("missing_scope_section_and_missing_citation_item", func(t *testing.T) {
		files := loadDirMap(t, filepath.Join(testdata, "moldloft", "decision-tree-clause-routing"))
		files["index.html"] = bytes.Replace(files["index.html"], []byte(`aria-labelledby="h-scope"`), []byte(`aria-labelledby="other"`), 1)
		files["index.html"] = bytes.Replace(files["index.html"], []byte(`id="cite-manual_part_15"`), []byte(`id="cite-removed"`), 1)
		recomputeMoldloftManifest(t, files)
		r := Map(context.Background(), files, Options{})
		assertHasRule(t, r, Fail, "CIT-SCOPE")
		assertHasRule(t, r, Fail, "CIT-INCOMPLETE")
	})

	t.Run("untested_citation_in_decision_tree", func(t *testing.T) {
		files := loadDirMap(t, filepath.Join(testdata, "moldloft", "decision-tree-clause-routing"))
		onlyMicro := `[
  {
    "id": "standard_micro",
    "label": "Micro-purchase without hazard endorsement",
    "answers": {
      "supplies_or_services": "no",
      "under_micro_limit": "yes"
    },
    "expect": {
      "outcome": "micro_purchase_ok",
      "status": "eligible",
      "steps": 2
    }
  }
]
`
		files["tests/worked-examples.json"] = []byte(onlyMicro)
		files["tests/worked-examples.js"] = []byte("(function () {\n  \"use strict\";\n  globalThis.MOLDLOFT_EXAMPLES = " + strings.TrimSpace(onlyMicro) + ";\n})();\n")
		recomputeMoldloftManifest(t, files)
		r := Map(context.Background(), files, Options{})
		assertHasRule(t, r, Fail, "EX-UNTESTED")
	})
}

