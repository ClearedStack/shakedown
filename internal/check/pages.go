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
	path     string
	policies []policyMeta
	markers  []marker
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
		pg := &page{path: p}
		pages[p] = pg
		loadSeen := false
		var walk func(n *htmlparse.Node, inHead bool)
		walk = func(n *htmlparse.Node, inHead bool) {
			if n.Type == htmlparse.ElementNode {
				c.scanElement(b, pg, n, inHead, &loadSeen)
				if n.Data == "head" {
					inHead = true
				}
			}
			for k := n.FirstChild; k != nil; k = k.NextSibling {
				walk(k, inHead)
			}
		}
		walk(doc, false)
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
		if _, hasSrc := attr(n, "src"); !hasSrc {
			t, _ := attr(n, "type")
			if scriptTypes[strings.ToLower(strings.TrimSpace(t))] && textOf(n) != "" {
				inline()
			}
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
