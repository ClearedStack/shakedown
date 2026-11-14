package check

import (
	"bytes"
	"strings"

	"github.com/ClearedStack/shakedown/internal/htmlparse"
)

const markerAttr = "data-shakedown"

// Markers a page uses to point at the parts the suite must find.
const (
	markDisclaimer = "disclaimer"
	markScope      = "scope-exclusions"
	markCitation   = "citation"
	markStorage    = "storage-disclosure"
	markClear      = "clear-data"
)

var knownMarkers = map[string]bool{markDisclaimer: true, markScope: true, markCitation: true, markStorage: true, markClear: true}

type marker struct {
	kind  string
	tag   string
	text  string
	attrs map[string]string
}

type policyMeta struct {
	content   string
	inHead    bool
	afterLoad bool
}

type page struct {
	path            string
	doc             *htmlparse.Node
	scriptSrcs      []string
	policies        []policyMeta
	markers         []marker
	hasScopeSection bool
	scopeItems      []string
	footerText      string
	citeItems       map[string]string
	citeLinks       map[string]bool
}

// loaders are the elements that can fetch or run something. A policy written
// after one of them does not govern it.
var loaders = map[string]bool{"script": true, "link": true, "style": true, "img": true, "iframe": true,
	"object": true, "embed": true, "video": true, "audio": true, "source": true}

// urlAttrs are the attributes that make the browser load or submit.
var urlAttrs = map[string]bool{"src": true, "href": true, "action": true, "formaction": true, "data": true,
	"poster": true, "manifest": true, "background": true, "codebase": true}

// executable script types; an empty type is JavaScript too.
var scriptTypes = map[string]bool{"": true, "text/javascript": true, "application/javascript": true, "module": true}

func attr(n *htmlparse.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

func textOf(n *htmlparse.Node) string {
	var b strings.Builder
	var walk func(*htmlparse.Node)
	walk = func(n *htmlparse.Node) {
		if n.Type == htmlparse.TextNode {
			b.WriteString(n.Data)
			b.WriteByte(' ')
		}
		for k := n.FirstChild; k != nil; k = k.NextSibling {
			walk(k)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

func (c *checker) scanPages(b *bundle) map[string]*page {
	pages := map[string]*page{}
	for _, p := range b.withExt(".html") {
		if c.done() {
			break
		}
		doc, err := htmlparse.Parse(bytes.NewReader(b.data[p]))
		if err != nil {
			c.add(wholeBundle, "BUNDLE-PAGE", EParse, p, "the page could not be read as HTML")
			continue
		}
		pg := &page{
			path:      p,
			doc:       doc,
			citeItems: map[string]string{},
			citeLinks: map[string]bool{},
		}
		pages[p] = pg
		loadSeen := false
		var walk func(n *htmlparse.Node, inHead, inScope, inSources bool)
		walk = func(n *htmlparse.Node, inHead, inScope, inSources bool) {
			if n.Type == htmlparse.ElementNode {
				c.scanElement(b, pg, n, inHead, &loadSeen)
				switch n.Data {
				case "head":
					inHead = true
				case "section":
					labelBy, _ := attr(n, "aria-labelledby")
					if labelBy == "h-scope" {
						pg.hasScopeSection = true
						inScope = true
					} else if labelBy == "h-sources" {
						inSources = true
					}
				case "li":
					if inScope {
						if txt := textOf(n); txt != "" {
							pg.scopeItems = append(pg.scopeItems, txt)
						}
					}
					if inSources {
						if id, ok := attr(n, "id"); ok && strings.HasPrefix(id, "cite-") {
							pg.citeItems[strings.TrimPrefix(id, "cite-")] = textOf(n)
						}
					}
				case "a":
					if href, ok := attr(n, "href"); ok && strings.HasPrefix(href, "#cite-") && textOf(n) != "" {
						pg.citeLinks[strings.TrimPrefix(href, "#cite-")] = true
					}
				case "footer":
					if txt := textOf(n); txt != "" {
						if pg.footerText == "" {
							pg.footerText = txt
						} else {
							pg.footerText += " " + txt
						}
					}
				}
			}
			for k := n.FirstChild; k != nil; k = k.NextSibling {
				walk(k, inHead, inScope, inSources)
			}
		}
		walk(doc, false, false, false)
	}
	return pages
}

func (c *checker) scanElement(b *bundle, pg *page, n *htmlparse.Node, inHead bool, loadSeen *bool) {
	p := pg.path
	inline := func() {
		c.add(CSP, "CSP-INLINE", ENeedsSubtool, p, "no rule covers inline script, inline style or inline event handlers")
	}
	unfamiliar := func(what string) {
		c.add(Offline, "OFF-UNFAMILIAR-REFERENCE", ENeedsSubtool, p, "no rule covers "+what)
	}

	switch n.Data {
	case "meta":
		equiv, _ := attr(n, "http-equiv")
		switch strings.ToLower(strings.TrimSpace(equiv)) {
		case "content-security-policy":
			content, _ := attr(n, "content")
			pg.policies = append(pg.policies, policyMeta{content: content, inHead: inHead, afterLoad: *loadSeen})
		case "refresh":
			unfamiliar("a meta refresh")
		}
	case "base":
		unfamiliar("a base element")
	case "script":
		t, _ := attr(n, "type")
		isExec := scriptTypes[strings.ToLower(strings.TrimSpace(t))]
		if src, hasSrc := attr(n, "src"); hasSrc {
			if isExec {
				if target, ok := b.resolveLocalRef(p, src); ok {
					pg.scriptSrcs = append(pg.scriptSrcs, target)
				}
			}
		} else if isExec && textOf(n) != "" {
			inline()
		}
	case "style":
		inline()
	case "input":
		if t, _ := attr(n, "type"); strings.EqualFold(strings.TrimSpace(t), "password") {
			c.add(Tracking, "TRK-SIGN-IN", EParse, p, "the page asks for a password")
		}
	}
	if loaders[n.Data] {
		*loadSeen = true
	}

	for _, a := range n.Attr {
		switch {
		case a.Key == "style" || strings.HasPrefix(a.Key, "on"):
			inline()
		case a.Key == "ping":
			c.add(Tracking, "TRK-BEACON", EParse, p, "a link carries a ping attribute")
		case a.Key == "srcset" || a.Key == "imagesrcset":
			unfamiliar("a srcset attribute")
		case a.Key == markerAttr:
			if !knownMarkers[a.Val] {
				c.add(wholeBundle, "BUNDLE-MARKER", ENeedsSubtool, p, "no rule covers a "+markerAttr+" value on this page")
				continue
			}
			m := marker{kind: a.Val, tag: n.Data, text: textOf(n), attrs: map[string]string{}}
			for _, x := range n.Attr {
				m.attrs[x.Key] = strings.TrimSpace(x.Val)
			}
			pg.markers = append(pg.markers, m)
		case urlAttrs[a.Key]:
			navigation := a.Key == "href" && (n.Data == "a" || n.Data == "area")
			c.reportRef(b, p, a.Val, navigation)
		}
	}
}

// scanMarkdown inspects bundled Markdown documentation (.md) for local or
// external link/image references and embedded HTML elements.
func (c *checker) scanMarkdown(b *bundle) {
	for _, p := range b.withExt(".md") {
		if c.done() {
			break
		}
		cleaned := stripMarkdownCode(string(b.data[p]))
		c.scanMarkdownLinks(b, p, cleaned)
		if hasRawHTMLTag(cleaned) {
			doc, err := htmlparse.Parse(strings.NewReader(cleaned))
			if err == nil {
				pg := &page{path: p, citeItems: map[string]string{}, citeLinks: map[string]bool{}}
				loadSeen := false
				var walk func(*htmlparse.Node)
				walk = func(n *htmlparse.Node) {
					if n.Type == htmlparse.ElementNode {
						c.scanElement(b, pg, n, false, &loadSeen)
					}
					for k := n.FirstChild; k != nil; k = k.NextSibling {
						walk(k)
					}
				}
				walk(doc)
			}
		}
	}
}

func stripMarkdownCode(md string) string {
	lines := strings.Split(md, "\n")
	var out strings.Builder
	inFence := false
	fenceChar := byte(0)
	prevBlank := true

	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if !inFence && (strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")) {
			inFence = true
			fenceChar = trimmed[0]
			out.WriteByte('\n')
			continue
		}
		if inFence {
			if strings.HasPrefix(trimmed, string([]byte{fenceChar, fenceChar, fenceChar})) {
				inFence = false
			}
			out.WriteByte('\n')
			continue
		}
		if prevBlank && (strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t")) {
			out.WriteByte('\n')
			continue
		}
		prevBlank = strings.TrimSpace(line) == ""
		out.WriteString(stripInlineCodeSpans(line))
		out.WriteByte('\n')
	}
	return out.String()
}

func stripInlineCodeSpans(line string) string {
	var b strings.Builder
	i := 0
	for i < len(line) {
		if line[i] != '`' {
			b.WriteByte(line[i])
			i++
			continue
		}
		ticks := 0
		for i+ticks < len(line) && line[i+ticks] == '`' {
			ticks++
		}
		delim := line[i : i+ticks]
		end := strings.Index(line[i+ticks:], delim)
		if end < 0 {
			b.WriteString(delim)
			i += ticks
			continue
		}
		b.WriteByte(' ')
		i += ticks + end + ticks
	}
	return b.String()
}

func (c *checker) scanMarkdownLinks(b *bundle, from, text string) {
	n := len(text)
	for i := 0; i < n; i++ {
		isImg := false
		if text[i] == '!' && i+1 < n && text[i+1] == '[' {
			isImg = true
			i++
		} else if text[i] != '[' {
			continue
		}
		closeBracket := strings.IndexByte(text[i+1:], ']')
		if closeBracket < 0 {
			continue
		}
		after := i + 1 + closeBracket + 1
		if after >= n || text[after] != '(' {
			continue
		}
		depth := 1
		j := after + 1
		for j < n && depth > 0 {
			switch text[j] {
			case '(':
				depth++
			case ')':
				depth--
			case '\n':
				depth = -1
			}
			if depth > 0 {
				j++
			}
		}
		if depth != 0 {
			continue
		}
		dest := strings.TrimSpace(text[after+1 : j])
		if strings.HasPrefix(dest, "<") && strings.HasSuffix(dest, ">") {
			dest = strings.TrimSpace(dest[1 : len(dest)-1])
		} else if sp := strings.IndexAny(dest, " \t"); sp >= 0 {
			dest = dest[:sp]
		}
		c.reportRef(b, from, dest, !isImg)
		i = j
	}
}

func hasRawHTMLTag(s string) bool {
	for i := 0; i+1 < len(s); i++ {
		if s[i] == '<' {
			ch := s[i+1]
			if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') {
				return true
			}
		}
	}
	return false
}

// reportRef applies the reference rules shared by pages and style sheets.
// A navigation link may leave the bundle; nothing that loads may.
func (c *checker) reportRef(b *bundle, from, raw string, navigation bool) {
	switch b.classifyRef(from, raw) {
	case refRemote:
		if !navigation {
			c.add(Offline, "OFF-REMOTE-REFERENCE", EParse, from, "loads or submits to an address outside the bundle")
		}
	case refMissing:
		c.add(Offline, "OFF-MISSING-REFERENCE", EParse, from, "refers to a file that is not in the bundle")
	case refOutside:
		c.add(Offline, "OFF-OUTSIDE-REFERENCE", EParse, from, "refers to a path above the bundle folder")
	case refInlineCode:
		c.add(CSP, "CSP-INLINE", ENeedsSubtool, from, "no rule covers inline script, inline style or inline event handlers")
	case refUnfamiliar:
		c.add(Offline, "OFF-UNFAMILIAR-REFERENCE", ENeedsSubtool, from, "no rule covers the form of an address on this page")
	}
}

func (pg *page) find(kind string) []marker {
	var out []marker
	if pg == nil {
		return out
	}
	for _, m := range pg.markers {
		if m.kind == kind {
			out = append(out, m)
		}
	}
	return out
}
