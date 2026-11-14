// Runs every worked example through the validator engine and checks exact
// equality against the expected validity, row count and error count.
(function (root) {
  "use strict";

  function run(tool, examples, engine) {
    return examples.map(function (example) {
      var result = engine.evaluate(tool, example.input);
      var problems = [];
      ["valid", "row_count", "error_count"].forEach(function (key) {
        if (result[key] !== example.expect[key]) {
          problems.push(key + ": expected " + example.expect[key] + ", got " + result[key]);
        }
      });
      return { id: example.id, label: example.label, ok: problems.length === 0, problems: problems };
    });
  }

  root.MoldloftCheck = { run: run };
})(globalThis);
