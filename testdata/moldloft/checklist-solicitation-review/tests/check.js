// Runs every worked example through the checklist engine and checks exact
// equality against the expected counts and status.
(function (root) {
  "use strict";

  function run(tool, examples, engine) {
    return examples.map(function (example) {
      var result = engine.evaluate(tool, example.answers);
      var problems = result.faults.map(function (f) {
        return f.id + ": " + f.kind;
      });
      var fields = ["status", "unmet_required", "yes_count", "no_count", "na_count"];
      fields.forEach(function (key) {
        if (result[key] !== example.expect[key]) {
          problems.push(key + ": expected " + example.expect[key] + ", got " + result[key]);
        }
      });
      return { id: example.id, label: example.label, ok: problems.length === 0, problems: problems };
    });
  }

  root.MoldloftCheck = { run: run };
})(globalThis);
