package htmlparse

import (
	"bytes"
	"testing"
)

func FuzzParseHTML(f *testing.F) {
	seeds := [][]byte{
		[]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Tool</title></head><body><p>Hello</p></body></html>`),
		[]byte(`<section aria-labelledby="h-scope"><h2 id="h-scope">Scope</h2><ul><li>Rule 1</li></ul></section>`),
		[]byte(`<!doctype html><html><head><script>if (a < b && c > d) {}</script><style>body { color: #111; }</style></head><body><a href="#cite-1">[1]</a></body></html>`),
		[]byte(`<!-- comment --><?pi test?><div data-shakedown="disclaimer" hidden>Text &amp; entities &#39;ok&#39;</div>`),
		[]byte(`<p>unclosed<li>one<li>two</ul`),
		[]byte(`<1<2<3<4</1></>`),
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 64<<10 {
			return
		}
		doc, err := Parse(bytes.NewReader(raw))
		if err != nil {
			return
		}
		if doc == nil || doc.Type != DocumentNode {
			t.Fatalf("unexpected root node: %+v", doc)
		}
	})
}
