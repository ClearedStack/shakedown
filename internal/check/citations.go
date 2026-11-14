package check

import (
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

func (c *checker) checkCitations(b *bundle, pages map[string]*page, entry string, calcs map[string]bool, examplesRead bool) {
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

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
