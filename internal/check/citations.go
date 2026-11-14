package check

import (
	"slices"
	"sort"
	"strings"
	"time"
)

// Phrases every copy of the MIT license carries.
var mitPhrases = []string{
	"MIT License",
	"Permission is hereby granted, free of charge, to any person obtaining a copy",
	`THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND`,
}

func (c *checker) checkCitations(b *bundle, m *manifest, pages map[string]*page, entry string, calcs map[string]bool, examplesRead bool) {
	if !b.exists[licenseName] {
		c.add(Citations, "CIT-LICENSE-MISSING", EParse, "", "the bundle has no LICENSE file")
	} else if data, ok := b.data[licenseName]; ok {
		text := strings.Join(strings.Fields(string(data)), " ")
		for _, phrase := range mitPhrases {
			if !strings.Contains(text, phrase) {
				c.add(Citations, "CIT-LICENSE-NOT-MIT", EParse, licenseName, "the LICENSE file is not the MIT license")
				break
			}
		}
	}

	if (m != nil && m.Kind == manifestMoldloft) || (m == nil && b.isMoldloft()) {
		c.checkMoldloftCitations(b, m, pages, entry, calcs, examplesRead)
		return
	}

	if pg := pages[entry]; pg != nil {
		filled := func(kind string) bool {
			for _, m := range pg.find(kind) {
				if m.text != "" {
					return true
				}
			}
			return false
		}
		if !filled(markDisclaimer) {
			c.add(Citations, "CIT-DISCLAIMER", EParse, entry, "the page has no non-advice disclaimer")
		}
		if !filled(markScope) {
			c.add(Citations, "CIT-SCOPE", EParse, entry, "the page does not state what is out of scope")
		}
	}

	paths := make([]string, 0, len(pages))
	for p := range pages {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	cited := map[string]bool{}
	found := false
	for _, p := range paths {
		for _, m := range pages[p].find(markCitation) {
			found = true
			calc := m.attrs["data-calculation"]
			_, dateErr := time.Parse("2006-01-02", m.attrs["data-revised"])
			if !idRE.MatchString(calc) || m.attrs["data-regulation"] == "" || m.attrs["data-paragraph"] == "" || dateErr != nil || m.text == "" {
				c.add(Citations, "CIT-INCOMPLETE", EParse, p, "a citation needs a calculation, a regulation, a paragraph, a revision date written YYYY-MM-DD, and text")
				continue
			}
			cited[calc] = true
		}
	}
	if !found {
		c.add(Citations, "CIT-MISSING", EParse, "", "no page cites a source")
	}
	if !examplesRead {
		return
	}
	for _, calc := range sortedKeys(calcs) {
		if !cited[calc] && found {
			c.add(Citations, "CIT-MISSING", EParse, "", "the calculation "+calc+" has no complete citation")
		}
	}
	for _, calc := range sortedKeys(cited) {
		if !calcs[calc] {
			c.add(Examples, "EX-UNTESTED", EParse, "", "the cited calculation "+calc+" has no worked example")
		}
	}
}

func (c *checker) checkMoldloftCitations(b *bundle, m *manifest, pages map[string]*page, entry string, exercised map[string]bool, examplesRead bool) {
	if entry == "" && b.exists["index.html"] {
		entry = "index.html"
	}
	pg := pages[entry]
	if pg != nil {
		if pg.footerText == "" {
			c.add(Citations, "CIT-DISCLAIMER", EParse, entry, "the page has no footer notice")
		}
		scopeOK := pg.hasScopeSection && len(pg.scopeItems) > 0
		if scopeOK && m != nil && m.Tool != nil {
			for _, want := range m.Tool.ScopeLimits {
				norm := strings.Join(strings.Fields(want), " ")
				if !slices.Contains(pg.scopeItems, norm) {
					scopeOK = false
					break
				}
			}
		}
		if !scopeOK {
			c.add(Citations, "CIT-SCOPE", EParse, entry, "the page does not state what is out of scope")
		}
	}

	if m == nil || m.Tool == nil {
		return
	}
	if m.Tool.InvalidCitationEntries {
		c.add(Citations, "CIT-INCOMPLETE", EParse, "data/tool.json", "every citation in data/tool.json needs a valid id, a label, and an optional http(s) url")
	}
	if len(m.Tool.Citations) == 0 && !m.Tool.InvalidCitationEntries {
		c.add(Citations, "CIT-MISSING", EParse, "data/tool.json", "data/tool.json cites no sources")
		return
	}
	for _, msg := range m.Tool.IncompleteCitations {
		c.add(Citations, "CIT-MISSING", EParse, "data/tool.json", msg)
	}

	if pg != nil {
		for _, cit := range m.Tool.Citations {
			normLabel := strings.Join(strings.Fields(cit.Label), " ")
			liText, hasLI := pg.citeItems[cit.ID]
			if !hasLI || !strings.Contains(liText, normLabel) {
				c.add(Citations, "CIT-INCOMPLETE", EParse, entry, "citation "+cit.ID+" is missing or incomplete in the page sources list")
				continue
			}
			if m.Tool.StaticLinkedCitations[cit.ID] && !pg.citeLinks[cit.ID] {
				c.add(Citations, "CIT-MISSING", EParse, entry, "citation "+cit.ID+" is not linked from the page body")
			}
		}
	}

	if !examplesRead {
		return
	}
	for _, cit := range m.Tool.Citations {
		if !exercised[cit.ID] {
			c.add(Examples, "EX-UNTESTED", EParse, "", "the cited rule "+cit.ID+" has no worked example")
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
