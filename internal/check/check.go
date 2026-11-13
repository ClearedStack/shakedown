package check

import (
	"context"
	"time"
)

// Options control one run. The zero value uses the defaults.
type Options struct {
	// CaseTimeout bounds each worked example, and the loading of the logic
	// scripts. Default two seconds.
	CaseTimeout time.Duration
}

type checker struct {
	ctx      context.Context
	opts     Options
	findings []Finding
	expired  bool
}

func (c *checker) add(condition, rule string, code Code, file, message string) {
	c.findings = append(c.findings, Finding{Condition: condition, Rule: rule, Code: code, File: file, Message: message})
}

// done reports whether the run's deadline has passed, and records that once.
func (c *checker) done() bool {
	if c.expired {
		return true
	}
	if c.ctx.Err() != nil {
		c.expired = true
		c.add(wholeBundle, "BUNDLE-TIMEOUT", ETimeout, "", "the check did not finish before its deadline")
	}
	return c.expired
}

// Dir checks the folder at dir against the five listing conditions. The
// error is non-nil only when the folder itself cannot be read.
func Dir(ctx context.Context, dir string, opts Options) (Report, error) {
	if opts.CaseTimeout <= 0 {
		opts.CaseTimeout = 2 * time.Second
	}
	c := &checker{ctx: ctx, opts: opts}
	if c.done() {
		return buildReport(c.findings), nil
	}
	b, err := c.load(dir)
	if err != nil {
		return Report{}, err
	}

	m := c.readManifest(b)
	c.checkJSONFiles(b)

	entry := ""
	if m != nil {
		entry = m.Entry
	}
	pages := c.scanPages(b)
	scripts := c.scanScripts(b)
	c.scanStyles(b)

	c.checkPolicies(pages)
	c.checkTracking(pages[entry], entry, scripts)
	calcs, examplesRead := c.runExamples(b, m)
	c.checkCitations(b, pages, entry, calcs, examplesRead)

	return buildReport(c.findings), nil
}
