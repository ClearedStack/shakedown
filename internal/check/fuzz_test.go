package check

import (
	"context"
	"testing"
	"time"
)

func FuzzCheckMap(f *testing.F) {
	f.Add(
		`{"contract":"v1","template":"calculator","entry":"index.html","scripts":["calc.js"],"function":"calculate","examples":"examples.json"}`,
		`<!doctype html><html><head><meta http-equiv="Content-Security-Policy" content="default-src 'self'; connect-src 'self'; form-action 'none'"></head><body><p data-shakedown="disclaimer">Not advice.</p><p data-shakedown="scope-exclusions">Synthetic.</p><div data-shakedown="citation" data-calculation="total" data-source="FAR" data-paragraph="1.1" data-revised="2025-01-01"></div><script src="calc.js"></script></body></html>`,
		`function calculate(x) { return { total: (x && x.n) || 0 }; }`,
		`[{"name":"w","kind":"worked","calculation":"total","input":{"n":1},"expected":{"total":1}},{"name":"b","kind":"boundary","calculation":"total","input":{"n":0},"expected":{"total":0}}]`,
		`# Sample Tool`+"\n\nSee [index](index.html).\n",
	)
	f.Add(
		`{"generator":"moldloft","generator_version":"v0.1.0-draft","contract_version":"v1","template":"checklist","item_id":"PA-0001","slug":"demo","spec_sha256":"0000000000000000000000000000000000000000000000000000000000000000","files":[]}`,
		`<!doctype html><html><head></head><body></body></html>`,
		`const x = 1;`,
		`[]`,
		`[bad](https://evil.example/x)`,
	)

	f.Fuzz(func(t *testing.T, manifestStr, htmlStr, jsStr, examplesStr, readmeStr string) {
		if len(manifestStr)+len(htmlStr)+len(jsStr)+len(examplesStr)+len(readmeStr) > 64*1024 {
			return
		}
		files := map[string][]byte{
			"LICENSE":       []byte("MIT License\nPermission is hereby granted, free of charge"),
			"index.html":    []byte(htmlStr),
			"calc.js":       []byte(jsStr),
			"examples.json": []byte(examplesStr),
			"README.md":     []byte(readmeStr),
		}
		if len(manifestStr) > 0 && manifestStr[0] == '{' && len(manifestStr)%2 == 0 {
			files["moldloft.json"] = []byte(manifestStr)
		} else {
			files["shakedown.json"] = []byte(manifestStr)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		r := Map(ctx, files, Options{CaseTimeout: 20 * time.Millisecond})
		if len(r.Conditions) != 5 {
			t.Fatalf("got %d conditions, want 5", len(r.Conditions))
		}
	})
}
