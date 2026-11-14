package js

import (
	"context"
	"testing"
	"time"
)

func FuzzParseAndRunJS(f *testing.F) {
	seeds := []string{
		`function compute(x) { return { y: (x && x.a ? x.a : 0) + 1 }; }`,
		`const m = new Map([["a", 10n], ["b", 20n]]); function compute() { return (m.get("a") + m.get("b")).toString(); }`,
		`globalThis.TOOL = { ok: true }; function compute(v) { return Array.from(String(v)).length; }`,
		`function compute(s) { return Object.prototype.hasOwnProperty.call({ k: 1 }, "k") ? String(s).padStart(4, "0") : ""; }`,
		`const a = []; a.push(a); function compute() { return String(a); }`,
		`const rx = /^([a-z]+)-([0-9]+)$/i; function compute(x) { return rx.test("abc-12"); }`,
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		if len(src) > 16<<10 {
			return
		}
		prog, err := Parse("fuzz.js", src)
		if err != nil {
			return
		}
		vm := NewVM()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		if err := vm.RunProgram(ctx, prog); err != nil {
			return
		}
		if vm.HasFunction("compute") {
			_, _ = vm.CallJSON(ctx, "compute", []byte(`{"a":1}`))
		}
	})
}
