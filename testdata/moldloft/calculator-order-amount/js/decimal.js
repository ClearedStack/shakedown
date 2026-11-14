// Exact decimal arithmetic for the calculator.
//
// A number is { units, scale }: the whole number `units` (a BigInt) divided by
// ten to the power `scale`. Nothing here uses floating point, so a result is
// the same in every browser. Rounding is always half away from zero:
// 0.125 to two places is 0.13, and -0.125 is -0.13.
(function (root) {
  "use strict";

  var PATTERN = /^-?[0-9]{1,15}(\.[0-9]{1,9})?$/;

  function pow10(n) {
    return 10n ** BigInt(n);
  }

  function abs(n) {
    return n < 0n ? -n : n;
  }

  // parse returns null for anything that is not a plain decimal: up to 15
  // digits, then optionally a point and up to 9 digits.
  function parse(text) {
    if (typeof text !== "string" || !PATTERN.test(text)) {
      return null;
    }
    var dot = text.indexOf(".");
    var scale = dot < 0 ? 0 : text.length - dot - 1;
    return { units: BigInt(text.replace(".", "")), scale: scale };
  }

  // format writes the number with exactly `scale` digits after the point.
  function format(d) {
    var digits = abs(d.units).toString();
    if (d.scale > 0) {
      digits = digits.padStart(d.scale + 1, "0");
      var cut = digits.length - d.scale;
      digits = digits.slice(0, cut) + "." + digits.slice(cut);
    }
    return d.units < 0n ? "-" + digits : digits;
  }

  // divRound divides and rounds half away from zero. `den` is never zero.
  function divRound(n, den) {
    var negative = (n < 0n) !== (den < 0n);
    var a = abs(n);
    var b = abs(den);
    var q = (2n * a + b) / (2n * b);
    return negative ? -q : q;
  }

  function rescale(d, scale) {
    if (scale >= d.scale) {
      return { units: d.units * pow10(scale - d.scale), scale: scale };
    }
    return { units: divRound(d.units, pow10(d.scale - scale)), scale: scale };
  }

  function cmp(a, b) {
    var s = Math.max(a.scale, b.scale);
    var x = rescale(a, s).units;
    var y = rescale(b, s).units;
    return x < y ? -1 : x > y ? 1 : 0;
  }

  function add(a, b) {
    var s = Math.max(a.scale, b.scale);
    return { units: rescale(a, s).units + rescale(b, s).units, scale: s };
  }

  function sub(a, b) {
    var s = Math.max(a.scale, b.scale);
    return { units: rescale(a, s).units - rescale(b, s).units, scale: s };
  }

  function mul(a, b) {
    return { units: a.units * b.units, scale: a.scale + b.scale };
  }

  // percent returns b percent of a.
  function percent(a, b) {
    return { units: a.units * b.units, scale: a.scale + b.scale + 2 };
  }

  // div returns a / b rounded to `scale` places. The caller checks b is not zero.
  function div(a, b, scale) {
    var n = a.units * pow10(b.scale + scale);
    var den = b.units * pow10(a.scale);
    return { units: divRound(n, den), scale: scale };
  }

  root.MoldloftDecimal = {
    parse: parse,
    format: format,
    rescale: rescale,
    cmp: cmp,
    add: add,
    sub: sub,
    mul: mul,
    percent: percent,
    div: div,
    isZero: function (d) { return d.units === 0n; },
    abs: function (d) { return { units: abs(d.units), scale: d.scale }; }
  };
})(globalThis);
