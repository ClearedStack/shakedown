// Runs every worked example through the decision-tree engine and checks exact
// equality against the expected outcome, status and step count.
(function (root) {
  "use strict";

  function run(tool, examples, engine) {
    return examples.map(function (example) {
      var result = engine.evaluate(tool, example.answers);
      var problems = [];
      if (!result.complete) {
        problems.push("path did not reach an outcome");
      }
      result.faults.forEach(function (f) {
        problems.push(f.question + ": " + f.kind);
      });
      ["outcome", "status", "steps"].forEach(function (key) {
        if (result[key] !== example.expect[key]) {
          problems.push(key + ": expected " + example.expect[key] + ", got " + result[key]);
        }
      });
      return { id: example.id, label: example.label, ok: problems.length === 0, problems: problems };
    });
  }

  root.MoldloftCheck = { run: run };
})(globalThis);
