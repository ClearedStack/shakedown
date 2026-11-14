package js

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/shakedown/internal/htmlparse"
)

func TestParseCollectsIdentifiersAndStrings(t *testing.T) {
	src := `
const greeting = "hello";
function compute(x) {
  const url = ` + "`https://bad.example/${x}`" + `;
  return fetch(url, greeting);
}
`
	prog, err := Parse("app.js", src)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	for _, want := range []string{"greeting", "compute", "fetch", "url"} {
		if !slices.Contains(prog.Identifiers, want) {
			t.Errorf("missing identifier %q in %v", want, prog.Identifiers)
		}
	}
	for _, want := range []string{"hello", "https://bad.example/"} {
		if !slices.Contains(prog.Strings, want) {
			t.Errorf("missing string %q in %v", want, prog.Strings)
		}
	}
}

func TestParseRejectsUnsupportedAndMalformed(t *testing.T) {
	unsupported := []string{
		"async function f() {}",
		"class Foo {}",
		"import x from 'y';",
		"export const a = 1;",
		"function* gen() { yield 1; }",
	}
	for _, src := range unsupported {
		_, err := Parse("test.js", src)
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("Parse(%q) = %v, want ErrUnsupported", src, err)
		}
	}

	malformed := []string{
		"function (",
		"const x = ;",
		"\"unterminated",
	}
	for _, src := range malformed {
		_, err := Parse("test.js", src)
		if !errors.Is(err, ErrSyntax) {
			t.Errorf("Parse(%q) = %v, want ErrSyntax", src, err)
		}
	}
}

func TestVMExecutionFeatures(t *testing.T) {
	src := `
function runAll(input) {
  // Hoisting and closures
  const add = makeAdder(input.base);
  function makeAdder(b) {
    return (x) => b + x;
  }

  // Loops, break, continue, array methods
  const nums = [1, 2, 3, 4, 5];
  let sum = 0;
  for (const n of nums) {
    if (n === 2) continue;
    if (n === 5) break;
    sum += add(n);
  }

  // Switch and try/catch/finally
  let label = "";
  switch (input.mode) {
    case "a":
      label = "alpha";
      break;
    default:
      label = "other";
  }

  let caught = false;
  let finalized = false;
  try {
    throw new Error("boom");
  } catch (e) {
    caught = e.message === "boom";
  } finally {
    finalized = true;
  }

  // Fixed UTC clock and JSON round-trip
  const epoch = Date.now();
  const iso = new Date().toISOString();
  const copy = JSON.parse(JSON.stringify({ sum: sum, mapped: nums.map(n => n * 2).filter(n => n > 4) }));

  return {
    sum: copy.sum,
    mapped: copy.mapped,
    label: label,
    caught: caught && finalized,
    epoch: epoch,
    iso: iso
  };
}
`
	prog, err := Parse("calc.js", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	vm := NewVM()
	if err := vm.RunProgram(context.Background(), prog); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !vm.HasFunction("runAll") {
		t.Fatal("HasFunction(runAll) = false")
	}
	got, err := vm.CallJSON(context.Background(), "runAll", []byte(`{"base":10,"mode":"a"}`))
	if err != nil {
		t.Fatalf("CallJSON: %v", err)
	}
	want := `{"sum":38,"mapped":[6,8,10],"label":"alpha","caught":true,"epoch":946684800000,"iso":"2000-01-01T00:00:00.000Z"}`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestVMDeterministicGuards(t *testing.T) {
	t.Run("Math.random throws", func(t *testing.T) {
		prog, err := Parse("rand.js", `function f() { return Math.random(); }`)
		if err != nil {
			t.Fatal(err)
		}
		vm := NewVM()
		if err := vm.RunProgram(context.Background(), prog); err != nil {
			t.Fatal(err)
		}
		_, err = vm.CallJSON(context.Background(), "f", []byte(`{}`))
		if !errors.Is(err, ErrRuntime) {
			t.Fatalf("got %v, want ErrRuntime", err)
		}
	})

	t.Run("recursion stack limit", func(t *testing.T) {
		prog, err := Parse("rec.js", `function f(n) { return f(n + 1); }`)
		if err != nil {
			t.Fatal(err)
		}
		vm := NewVM()
		if err := vm.RunProgram(context.Background(), prog); err != nil {
			t.Fatal(err)
		}
		_, err = vm.CallJSON(context.Background(), "f", []byte(`0`))
		if !errors.Is(err, ErrRuntime) {
			t.Fatalf("got %v, want ErrRuntime", err)
		}
	})

	t.Run("deadline interruption", func(t *testing.T) {
		prog, err := Parse("spin.js", `function f() { while (true) {} }`)
		if err != nil {
			t.Fatal(err)
		}
		vm := NewVM()
		if err := vm.RunProgram(context.Background(), prog); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err = vm.CallJSON(ctx, "f", []byte(`null`))
		if !errors.Is(err, ErrInterrupted) {
			t.Fatalf("got %v, want ErrInterrupted", err)
		}
	})
}

func TestVMBigIntMapAndMoldloftBuiltins(t *testing.T) {
	src := `
globalThis.MOLDLOFT_DATA = { prefix: "USD", scale: 4 };

function runCheck(input) {
  const scale = BigInt(MOLDLOFT_DATA.scale);
  const factor = 10n ** scale;
  const a = BigInt(input.whole) * factor + BigInt(input.frac.padEnd(4, "0").slice(0, 4));
  const half = 5000n;
  const rounded = (a + half) / factor;
  const neg = (-7n) / 2n;
  const rem = (-7n) % 2n;

  const seen = new Map();
  seen.set("total", rounded);
  seen.set("hex", 0xffn);

  const hasWhole = Object.prototype.hasOwnProperty.call(input, "whole");
  const hasMissing = Object.prototype.hasOwnProperty.call(input, "missing");
  const codePoints = Array.from(input.label).length;
  const utf16Units = input.label.length;

  let mapKeys = [];
  for (const entry of seen) {
    mapKeys.push(entry[0] + "=" + entry[1].toString());
  }

  return {
    prefix: globalThis.MOLDLOFT_DATA.prefix,
    rounded: seen.get("total").toString(),
    neg: neg.toString(),
    rem: rem.toString(),
    hasHex: seen.has("hex"),
    size: seen.size,
    hasWhole: hasWhole,
    hasMissing: hasMissing,
    codePoints: codePoints,
    utf16Units: utf16Units,
    entries: mapKeys
  };
}
`
	prog, err := Parse("moldloft.js", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	vm := NewVM()
	if err := vm.RunProgram(context.Background(), prog); err != nil {
		t.Fatalf("RunProgram: %v", err)
	}
	got, err := vm.CallJSON(context.Background(), "runCheck", []byte(`{"whole":12,"frac":"55","label":"A\ud83d\ude00B"}`))
	if err != nil {
		t.Fatalf("CallJSON: %v", err)
	}
	want := `{"prefix":"USD","rounded":"13","neg":"-3","rem":"-1","hasHex":true,"size":2,"hasWhole":true,"hasMissing":false,"codePoints":3,"utf16Units":4,"entries":["total=13","hex=255"]}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}

	evalOut, err := vm.EvalJSON(context.Background(), `globalThis.MOLDLOFT_DATA`)
	if err != nil {
		t.Fatalf("EvalJSON: %v", err)
	}
	if evalOut != `{"prefix":"USD","scale":4}` {
		t.Fatalf("EvalJSON = %s", evalOut)
	}
}

func TestVMMemoryCapsThrowCleanly(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "string doubling cap",
			src: `function f() {
				let s = "abcdefgh";
				for (let i = 0; i < 30; i++) { s = s + s; }
				return s.length;
			}`,
		},
		{
			name: "string repeat cap",
			src:  `function f() { return "abcd".repeat(200000); }`,
		},
		{
			name: "padStart cap",
			src:  `function f() { return "x".padStart(500000, "0"); }`,
		},
		{
			name: "array push cap",
			src: `function f() {
				const a = [];
				for (let i = 0; i < 20000; i++) { a.push(i); }
				return a.length;
			}`,
		},
		{
			name: "Array constructor cap",
			src:  `function f() { return Array(20000).length; }`,
		},
		{
			name: "object property cap",
			src: `function f() {
				const o = {};
				for (let i = 0; i < 5000; i++) { o["k" + i] = i; }
				return 1;
			}`,
		},
		{
			name: "BigInt exponentiation cap",
			src:  `function f() { return (2n ** 20000n).toString(); }`,
		},
		{
			name: "VM total allocation budget",
			src: `function f() {
				let last = null;
				for (let i = 0; i < 100000; i++) { last = { i: i }; }
				return last;
			}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prog, err := Parse("cap.js", tc.src)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			vm := NewVM()
			if err := vm.RunProgram(context.Background(), prog); err != nil {
				t.Fatalf("RunProgram: %v", err)
			}
			_, err = vm.CallJSON(context.Background(), "f", []byte(`null`))
			if !errors.Is(err, ErrRuntime) {
				t.Fatalf("got %v, want ErrRuntime", err)
			}
		})
	}
}

func TestAttachDOMAndPassingTool(t *testing.T) {
	toolDir := filepath.Join("..", "..", "testdata", "tools", "passing-tool")
	htmlRaw, err := os.ReadFile(filepath.Join(toolDir, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := htmlparse.Parse(bytes.NewReader(htmlRaw))
	if err != nil {
		t.Fatal(err)
	}

	vm := NewVM()
	dom, err := vm.AttachDOM(doc)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	for _, name := range []string{"tables.js", "calc.js", "app.js"} {
		src, err := os.ReadFile(filepath.Join(toolDir, name))
		if err != nil {
			t.Fatal(err)
		}
		prog, err := Parse(name, string(src))
		if err != nil {
			t.Fatalf("Parse(%s): %v", name, err)
		}
		if err := vm.RunProgram(ctx, prog); err != nil {
			t.Fatalf("RunProgram(%s): %v", name, err)
		}
	}

	if err := dom.SetInputValue(ctx, "crates", "10"); err != nil {
		t.Fatal(err)
	}
	if err := dom.SetInputValue(ctx, "days", "45"); err != nil {
		t.Fatal(err)
	}
	if err := dom.Click(ctx, "go"); err != nil {
		t.Fatal(err)
	}

	resText := dom.ElementByID("result").TextContent()
	if !strings.HasPrefix(resText, "Fee: ") {
		t.Fatalf("result text = %q, want Fee: ...", resText)
	}
	saved, ok := dom.LocalStorage("crate-storage-fee")
	if !ok || !strings.Contains(saved, `"crates":10`) {
		t.Fatalf("localStorage = %q (ok=%v), want saved JSON", saved, ok)
	}

	if err := dom.Click(ctx, "clear"); err != nil {
		t.Fatal(err)
	}
	if _, ok := dom.LocalStorage("crate-storage-fee"); ok {
		t.Fatal("expected localStorage entry to be removed by #clear click")
	}
	if got := dom.ElementByID("result").TextContent(); got != "Saved data cleared." {
		t.Fatalf("result text after clear = %q, want Saved data cleared.", got)
	}
}
