package check

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/ClearedStack/shakedown/internal/js"
)

var decisionStatusLabels = map[string]string{
	"eligible":   "Eligible",
	"ineligible": "Not eligible",
	"review":     "Further review required",
}

func (c *checker) verifyMoldloftUI(b *bundle, m *manifest, pages map[string]*page, cases []moldloftCase) {
	if c.done() || m == nil || len(cases) == 0 {
		return
	}
	if testPage := pages["tests/index.html"]; testPage != nil && testPage.doc != nil {
		c.verifyMoldloftBrowserRunner(b, testPage, len(cases))
	}
	if c.done() {
		return
	}
	if entryPage := pages[m.Entry]; entryPage != nil && entryPage.doc != nil {
		c.verifyMoldloftEntryPage(b, m, entryPage, cases)
	}
}

func (c *checker) loadPageDOM(ctx context.Context, b *bundle, pg *page) (*js.DOM, bool) {
	if len(pg.scriptSrcs) == 0 {
		c.add(Examples, "EX-SCRIPT", EParse, pg.path, "the page does not load any scripts")
		return nil, false
	}
	vm := js.NewVM()
	dom, err := vm.AttachDOM(pg.doc)
	if err != nil {
		c.add(Examples, "EX-THREW", EParse, pg.path, "the page DOM could not be initialized")
		return nil, false
	}
	for _, src := range pg.scriptSrcs {
		raw, ok := b.data[src]
		if !ok {
			c.add(Examples, "EX-SCRIPT", EParse, src, "the script referenced by "+pg.path+" could not be loaded")
			return nil, false
		}
		prog, err := js.Parse(src, string(raw))
		if err != nil {
			c.add(Examples, "EX-SCRIPT", EParse, src, "the script could not be parsed")
			return nil, false
		}
		if err := vm.RunProgram(ctx, prog); err != nil {
			if errors.Is(err, js.ErrInterrupted) {
				c.add(Examples, "EX-TIMEOUT", ETimeout, pg.path, "the page scripts did not finish before their deadline")
			} else {
				c.add(Examples, "EX-THREW", EParse, src, "the page script raised an error during startup")
			}
			return nil, false
		}
	}
	if err := dom.DrainTimers(ctx); err != nil {
		if errors.Is(err, js.ErrInterrupted) {
			c.add(Examples, "EX-TIMEOUT", ETimeout, pg.path, "the page timers did not finish before their deadline")
		} else {
			c.add(Examples, "EX-THREW", EParse, pg.path, "a page timer raised an error during startup")
		}
		return nil, false
	}
	return dom, true
}

func (c *checker) verifyMoldloftBrowserRunner(b *bundle, pg *page, wantCount int) {
	runCtx, cancel := context.WithTimeout(c.ctx, c.opts.CaseTimeout)
	defer cancel()

	dom, ok := c.loadPageDOM(runCtx, b, pg)
	if !ok {
		return
	}

	summary := dom.ElementByID("summary")
	results := dom.ElementByID("results")
	if summary == nil || results == nil {
		c.add(Examples, "EX-THREW", EParse, pg.path, "tests/index.html is missing #summary or #results")
		return
	}
	if summary.ClassName() != "pass" || strings.TrimSpace(summary.TextContent()) == "" {
		c.add(Examples, "EX-MISMATCH", EParse, pg.path, "browser test runner summary did not report pass")
		return
	}
	items := results.Children()
	if len(items) != wantCount {
		c.add(Examples, "EX-MISMATCH", EParse, pg.path, "browser test runner rendered a different number of worked examples")
		return
	}
	for _, it := range items {
		if it.TagName() != "LI" || it.ClassName() != "pass" {
			c.add(Examples, "EX-MISMATCH", EParse, pg.path, "browser test runner reported a failing worked example")
			return
		}
	}
}

func (c *checker) verifyMoldloftEntryPage(b *bundle, m *manifest, pg *page, cases []moldloftCase) {
	initCtx, cancelInit := context.WithTimeout(c.ctx, c.opts.CaseTimeout)
	dom, ok := c.loadPageDOM(initCtx, b, pg)
	cancelInit()
	if !ok {
		return
	}

	statusEl := dom.ElementByID("status")
	if statusEl == nil || strings.TrimSpace(statusEl.TextContent()) == "" {
		c.add(Examples, "EX-THREW", EParse, pg.path, "entry page is missing a populated #status region")
		return
	}

	for _, mc := range cases {
		if c.done() {
			return
		}
		if interrupted := c.driveMoldloftUICase(m, pg.path, dom, mc); interrupted {
			return
		}
	}

	resetCtx, cancelReset := context.WithTimeout(c.ctx, c.opts.CaseTimeout)
	defer cancelReset()
	if err := dom.ResetForm(resetCtx, "inputs"); err != nil {
		c.recordUIDriverErr(pg.path, "form reset", err)
		return
	}
	if statusAfter := dom.ElementByID("status"); statusAfter == nil || strings.TrimSpace(statusAfter.TextContent()) == "" {
		c.add(Examples, "EX-MISMATCH", EParse, pg.path, "entry page cleared #status on form reset")
	}
}

func (c *checker) recordUIDriverErr(file, where string, err error) bool {
	if errors.Is(err, js.ErrInterrupted) {
		c.add(Examples, "EX-TIMEOUT", ETimeout, file, "UI "+where+" did not finish before its deadline")
		return true
	}
	c.add(Examples, "EX-THREW", EParse, file, "UI "+where+" raised an error")
	return false
}

func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (c *checker) driveMoldloftUICase(m *manifest, pagePath string, dom *js.DOM, mc moldloftCase) bool {
	caseCtx, cancel := context.WithTimeout(c.ctx, c.opts.CaseTimeout)
	defer cancel()

	if err := dom.ResetForm(caseCtx, "inputs"); err != nil {
		return c.recordUIDriverErr(pagePath, "case "+mc.id+" reset", err)
	}

	switch m.Template {
	case "calculator":
		for _, k := range sortedMapKeys(mc.answersMap) {
			if err := dom.SetInputValue(caseCtx, "in-"+k, mc.answersMap[k]); err != nil {
				return c.recordUIDriverErr(pagePath, "case "+mc.id+" input "+k, err)
			}
		}
		for _, stepID := range sortedMapKeys(mc.expectMap) {
			if stepID == "faults" {
				continue
			}
			wantStr, ok := mc.expectMap[stepID].(string)
			if !ok {
				continue
			}
			outEl := dom.ElementByID("out-" + stepID)
			if outEl == nil {
				c.add(Examples, "EX-THREW", EParse, pagePath, "entry page is missing #out-"+stepID)
				return false
			}
			if strings.TrimSpace(outEl.TextContent()) != wantStr {
				c.add(Examples, "EX-MISMATCH", EParse, pagePath, "UI output #out-"+stepID+" did not match case "+mc.id)
				return false
			}
		}

	case "checklist":
		for _, k := range sortedMapKeys(mc.answersMap) {
			if err := dom.SetInputValue(caseCtx, "in-"+k, mc.answersMap[k]); err != nil {
				return c.recordUIDriverErr(pagePath, "case "+mc.id+" answer "+k, err)
			}
		}
		if wantStatus, ok := mc.expectMap["status"].(string); ok {
			outStatus := dom.ElementByID("out-status")
			if outStatus == nil {
				c.add(Examples, "EX-THREW", EParse, pagePath, "entry page is missing #out-status")
				return false
			}
			if strings.TrimSpace(outStatus.TextContent()) != wantStatus {
				c.add(Examples, "EX-MISMATCH", EParse, pagePath, "UI #out-status did not match case "+mc.id)
				return false
			}
		}
		if mc.expectMap["faults"] == nil {
			pairs := [][2]string{
				{"unmet_required", "out-unmet"},
				{"yes_count", "out-yes"},
				{"no_count", "out-no"},
				{"na_count", "out-na"},
			}
			for _, pair := range pairs {
				num, ok := mc.expectMap[pair[0]].(float64)
				if !ok {
					continue
				}
				el := dom.ElementByID(pair[1])
				if el == nil {
					c.add(Examples, "EX-THREW", EParse, pagePath, "entry page is missing #"+pair[1])
					return false
				}
				if strings.TrimSpace(el.TextContent()) != strconv.Itoa(int(num)) {
					c.add(Examples, "EX-MISMATCH", EParse, pagePath, "UI #"+pair[1]+" did not match case "+mc.id)
					return false
				}
			}
		}

	case "decision-tree":
		if m.Tool != nil {
			visited := map[string]bool{}
			cur := m.Tool.StartQuestion
			for cur != "" && !visited[cur] {
				visited[cur] = true
				qBox := dom.ElementByID("q-" + cur)
				if qBox == nil {
					c.add(Examples, "EX-THREW", EParse, pagePath, "entry page is missing #q-"+cur)
					return false
				}
				if qBox.Hidden() {
					c.add(Examples, "EX-MISMATCH", EParse, pagePath, "question #q-"+cur+" remained hidden in case "+mc.id)
					return false
				}
				ans, hasAns := mc.answersMap[cur]
				if !hasAns {
					break
				}
				if err := dom.SetInputValue(caseCtx, "in-"+cur, ans); err != nil {
					return c.recordUIDriverErr(pagePath, "case "+mc.id+" question "+cur, err)
				}
				opts := m.Tool.QuestionOptions[cur]
				if opts == nil {
					break
				}
				next := opts[ans]
				if m.Tool.QuestionOptions[next] == nil {
					break
				}
				cur = next
			}
		}
		if stepsNum, ok := mc.expectMap["steps"].(float64); ok {
			outSteps := dom.ElementByID("out-steps")
			if outSteps == nil {
				c.add(Examples, "EX-THREW", EParse, pagePath, "entry page is missing #out-steps")
				return false
			}
			if strings.TrimSpace(outSteps.TextContent()) != strconv.Itoa(int(stepsNum)) {
				c.add(Examples, "EX-MISMATCH", EParse, pagePath, "UI #out-steps did not match case "+mc.id)
				return false
			}
		}
		if wantStatus, ok := mc.expectMap["status"].(string); ok && wantStatus != "" {
			wantLabel := decisionStatusLabels[wantStatus]
			if wantLabel == "" {
				wantLabel = wantStatus
			}
			outStatus := dom.ElementByID("out-status")
			outOutcome := dom.ElementByID("out-outcome")
			if outStatus == nil || outOutcome == nil {
				c.add(Examples, "EX-THREW", EParse, pagePath, "entry page is missing #out-status or #out-outcome")
				return false
			}
			outcomeText := strings.TrimSpace(outOutcome.TextContent())
			if strings.TrimSpace(outStatus.TextContent()) != wantLabel || outcomeText == "" || outcomeText == "-" {
				c.add(Examples, "EX-MISMATCH", EParse, pagePath, "UI decision outcome did not match case "+mc.id)
				return false
			}
		}

	case "validator":
		if err := dom.SetInputValue(caseCtx, "in-text", mc.inputStr); err != nil {
			return c.recordUIDriverErr(pagePath, "case "+mc.id+" input", err)
		}
		if wantValid, ok := mc.expectMap["valid"].(bool); ok {
			wantStatus := "Invalid"
			if wantValid {
				wantStatus = "Valid"
			}
			outStatus := dom.ElementByID("out-status")
			findings := dom.ElementByID("findings")
			if outStatus == nil || findings == nil {
				c.add(Examples, "EX-THREW", EParse, pagePath, "entry page is missing #out-status or #findings")
				return false
			}
			if strings.TrimSpace(outStatus.TextContent()) != wantStatus {
				c.add(Examples, "EX-MISMATCH", EParse, pagePath, "UI #out-status did not match case "+mc.id)
				return false
			}
			items := findings.Children()
			if wantValid {
				if len(items) != 1 || items[0].ClassName() != "pass" {
					c.add(Examples, "EX-MISMATCH", EParse, pagePath, "UI #findings did not show passing item for case "+mc.id)
					return false
				}
			} else if errCount, ok := mc.expectMap["error_count"].(float64); ok && int(errCount) > 0 {
				if len(items) != int(errCount) {
					c.add(Examples, "EX-MISMATCH", EParse, pagePath, "UI #findings count did not match case "+mc.id)
					return false
				}
				for _, li := range items {
					if li.ClassName() != "fail" {
						c.add(Examples, "EX-MISMATCH", EParse, pagePath, "UI #findings item missing fail class for case "+mc.id)
						return false
					}
				}
			}
		}
		for _, pair := range [][2]string{{"row_count", "out-rows"}, {"error_count", "out-errors"}} {
			if num, ok := mc.expectMap[pair[0]].(float64); ok {
				el := dom.ElementByID(pair[1])
				if el == nil {
					c.add(Examples, "EX-THREW", EParse, pagePath, "entry page is missing #"+pair[1])
					return false
				}
				if strings.TrimSpace(el.TextContent()) != strconv.Itoa(int(num)) {
					c.add(Examples, "EX-MISMATCH", EParse, pagePath, "UI #"+pair[1]+" did not match case "+mc.id)
					return false
				}
			}
		}
	}
	return false
}
