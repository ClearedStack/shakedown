// Works the calculator out from its definition (js/data.js) and a set of raw
// input strings. It touches neither the page nor the network.
(function (root) {
  "use strict";

  var D = root.MoldloftDecimal;

  // A result may not reach 10^15.
  var LIMIT = { units: 10n ** 15n, scale: 0 };

  function findTable(tool, id) {
    for (var i = 0; i < tool.tables.length; i++) {
      if (tool.tables[i].id === id) {
        return tool.tables[i];
      }
    }
    return null;
  }

  // readInput returns { value } or { kind }, where kind says what is wrong.
  function readInput(tool, input, text) {
    if (typeof text !== "string" || text === "") {
      return { kind: "missing" };
    }
    if (input.kind === "choice") {
      var rows = findTable(tool, input.table).rows;
      for (var i = 0; i < rows.length; i++) {
        if (rows[i].key === text) {
          return { value: D.parse(rows[i].value) };
        }
      }
      return { kind: "choice" };
    }
    var d = D.parse(text);
    if (d === null) {
      return { kind: "format" };
    }
    if (d.scale > input.decimals) {
      return { kind: "decimals" };
    }
    if (D.cmp(d, D.parse(input.min)) < 0 || D.cmp(d, D.parse(input.max)) > 0) {
      return { kind: "range" };
    }
    return { value: D.rescale(d, input.decimals) };
  }

  // runStep returns { value } or { kind }.
  function runStep(step, args) {
    var acc = args[0];
    var i;
    switch (step.op) {
      case "add":
        for (i = 1; i < args.length; i++) { acc = D.add(acc, args[i]); }
        break;
      case "sub":
        acc = D.sub(acc, args[1]);
        break;
      case "mul":
        for (i = 1; i < args.length; i++) { acc = D.mul(acc, args[i]); }
        break;
      case "pct":
        acc = D.percent(acc, args[1]);
        break;
      case "div":
        if (D.isZero(args[1])) {
          return { kind: "divide_by_zero" };
        }
        acc = D.div(acc, args[1], step.decimals);
        break;
      case "min":
        for (i = 1; i < args.length; i++) { if (D.cmp(args[i], acc) < 0) { acc = args[i]; } }
        break;
      case "max":
        for (i = 1; i < args.length; i++) { if (D.cmp(args[i], acc) > 0) { acc = args[i]; } }
        break;
      default:
        throw new Error("unknown operation");
    }
    acc = D.rescale(acc, step.decimals);
    if (D.cmp(D.abs(acc), LIMIT) >= 0) {
      return { kind: "overflow" };
    }
    return { value: acc };
  }

  // evaluate returns { values, faults }. `values` maps the id of every input,
  // constant and step that could be worked out to its decimal text. `faults`
  // lists { id, kind } for every input refused and every step that failed. A
  // step that depends on something that failed is left out of both.
  function evaluate(tool, raw) {
    var known = new Map();
    var values = {};
    var faults = [];

    function keep(id, d) {
      known.set(id, d);
      values[id] = D.format(d);
    }

    tool.inputs.forEach(function (input) {
      var text = Object.prototype.hasOwnProperty.call(raw, input.id) ? raw[input.id] : "";
      var got = readInput(tool, input, text);
      if (got.kind) {
        faults.push({ id: input.id, kind: got.kind });
      } else {
        keep(input.id, got.value);
      }
    });
    tool.constants.forEach(function (c) {
      keep(c.id, D.parse(c.value));
    });
    tool.steps.forEach(function (step) {
      var args = [];
      for (var i = 0; i < step.args.length; i++) {
        if (!known.has(step.args[i])) {
          return;
        }
        args.push(known.get(step.args[i]));
      }
      var got = runStep(step, args);
      if (got.kind) {
        faults.push({ id: step.id, kind: got.kind });
      } else {
        keep(step.id, got.value);
      }
    });
    return { values: values, faults: faults };
  }

  root.MoldloftEngine = { evaluate: evaluate };
})(globalThis);
