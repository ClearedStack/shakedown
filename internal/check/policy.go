package check

import (
	"regexp"
	"sort"
	"strings"
)

var directiveRE = regexp.MustCompile(`^[a-z0-9-]+$`)

// required are the three directives the specification names, with the one
// value each must have.
var required = []struct{ name, value, rule string }{
	{"default-src", "'self'", "CSP-DEFAULT-SRC"},
	{"connect-src", "'self'", "CSP-CONNECT-SRC"},
	{"form-action", "'none'", "CSP-FORM-ACTION"},
}

// sourceDirectives take a list of sources. Any other directive name, apart
// from the valueless ones below, has no rule.
var sourceDirectives = map[string]bool{"script-src": true, "script-src-elem": true, "script-src-attr": true,
	"style-src": true, "style-src-elem": true, "style-src-attr": true, "img-src": true, "font-src": true,
	"media-src": true, "object-src": true, "frame-src": true, "child-src": true, "worker-src": true,
	"manifest-src": true, "base-uri": true, "frame-ancestors": true}

var flagDirectives = map[string]bool{"upgrade-insecure-requests": true, "block-all-mixed-content": true}

var localSchemes = map[string]bool{"data:": true, "blob:": true, "filesystem:": true, "mediastream:": true}

func (c *checker) checkPolicies(pages map[string]*page) {
	paths := make([]string, 0, len(pages))
	for p := range pages {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		pg := pages[p]
		switch len(pg.policies) {
		case 0:
			c.add(CSP, "CSP-MISSING", EParse, p, "the page has no Content-Security-Policy meta element")
			continue
		case 1:
		default:
			c.add(CSP, "CSP-MULTIPLE", EParse, p, "the page has more than one Content-Security-Policy meta element")
			continue
		}
		pol := pg.policies[0]
		if !pol.inHead || pol.afterLoad {
			c.add(CSP, "CSP-PLACEMENT", EParse, p, "the policy must sit in the head, before anything that loads")
		}
		c.checkPolicy(p, pol.content)
	}
}

func (c *checker) checkPolicy(p, content string) {
	invalid := func(msg string) { c.add(CSP, "CSP-INVALID", EParse, p, msg) }
	if strings.Contains(content, ",") {
		invalid("the policy contains a comma")
		return
	}
	directives := map[string][]string{}
	for _, part := range strings.Split(content, ";") {
		fields := strings.Fields(part)
		if len(fields) == 0 {
			continue
		}
		name := strings.ToLower(fields[0])
		if !directiveRE.MatchString(name) {
			invalid("the policy has a badly formed directive name")
			return
		}
		if _, dup := directives[name]; dup {
			invalid("the policy repeats a directive")
			return
		}
		values := []string{}
		for _, v := range fields[1:] {
			values = append(values, strings.ToLower(v))
		}
		directives[name] = values
	}
	if len(directives) == 0 {
		invalid("the policy is empty")
		return
	}
	for _, r := range required {
		if v, ok := directives[r.name]; !ok || len(v) != 1 || v[0] != r.value {
			c.add(CSP, r.rule, EParse, p, r.name+" must be exactly "+r.value)
		}
		delete(directives, r.name)
	}
	for name, values := range directives {
		if flagDirectives[name] && len(values) == 0 {
			continue
		}
		if !sourceDirectives[name] {
			c.add(CSP, "CSP-UNFAMILIAR", ENeedsSubtool, p, "no rule covers a directive in the policy")
			continue
		}
		for _, v := range values {
			switch {
			case v == "'self'" || v == "'none'":
			case strings.HasPrefix(v, "'") || localSchemes[v]:
				c.add(CSP, "CSP-UNFAMILIAR", ENeedsSubtool, p, "no rule covers a source in the policy")
			default:
				c.add(CSP, "CSP-REMOTE-SOURCE", EParse, p, name+" allows a source outside the bundle")
			}
		}
	}
}
