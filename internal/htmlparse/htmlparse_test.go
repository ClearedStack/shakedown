package htmlparse

import (
	"errors"
	"strings"
	"testing"
)

func TestParseBasicAndEntities(t *testing.T) {
	src := `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="Content-Security-Policy" content="default-src &#39;self&#39;; connect-src &#39;self&#39;">
<title>Test &amp; Demo</title>
<script>for (var i = 0; i < 10; i++) {}</script>
</head>
<body>
<p data-shakedown="disclaimer">General &lt;info&gt; only.</p>
<p>Second paragraph</p>
</body>
</html>`
	doc, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if doc.Type != DocumentNode {
		t.Fatalf("expected DocumentNode, got %v", doc.Type)
	}
	var metas int
	var walk func(*Node)
	walk = func(n *Node) {
		if n.Type == ElementNode && n.Data == "meta" {
			metas++
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if metas != 2 {
		t.Fatalf("got %d meta elements, want 2", metas)
	}
}

func TestParseTooDeep(t *testing.T) {
	var b strings.Builder
	b.WriteString("<!doctype html><html><body>")
	for i := 0; i < 600; i++ {
		b.WriteString("<div>")
	}
	_, err := Parse(strings.NewReader(b.String()))
	if !errors.Is(err, ErrTooDeep) {
		t.Fatalf("got %v, want ErrTooDeep", err)
	}
}
