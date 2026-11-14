// Runs every worked example through the engine and reports, for each one,
// whether the results match exactly. Used by tests/run.js (Node.js) and by
// tests/index.html (any browser).
(function (root) {
  "use strict";

  function run(tool, examples, engine) {
    return examples.map(function (example) {
      var result = engine.evaluate(tool, example.inputs);
      var problems = result.faults.map(function (f) {
        return f.id + ": " + f.kind;
      });
      Object.keys(example.expect).sort().forEach(function (id) {
        var got = Object.prototype.hasOwnProperty.call(result.values, id) ? result.values[id] : "nothing";
        if (got !== example.expect[id]) {
          problems.push(id + ": expected " + example.expect[id] + ", got " + got);
        }
      });
      return { id: example.id, label: example.label, ok: problems.length === 0, problems: problems };
    });
  }

  root.MoldloftCheck = { run: run };
})(globalThis);
