// Runs the worked examples with Node.js:  node tests/run.js
"use strict";

var fs = require("fs");
var path = require("path");

require("../js/data.js");
require("../js/engine.js");
require("./worked-examples.js");
require("./check.js");

var failed = 0;

function report(ok, name, problems) {
  console.log((ok ? "ok   " : "FAIL ") + name);
  (problems || []).forEach(function (p) {
    console.log("       " + p);
  });
  if (!ok) {
    failed++;
  }
}

function sameAsJSON(file, value) {
  var onDisk = JSON.parse(fs.readFileSync(path.join(__dirname, "..", file), "utf8"));
  return JSON.stringify(onDisk) === JSON.stringify(value);
}
report(sameAsJSON("data/tool.json", globalThis.MOLDLOFT_TOOL), "js/data.js matches data/tool.json");
report(sameAsJSON("tests/worked-examples.json", globalThis.MOLDLOFT_EXAMPLES), "tests/worked-examples.js matches tests/worked-examples.json");

globalThis.MoldloftCheck.run(globalThis.MOLDLOFT_TOOL, globalThis.MOLDLOFT_EXAMPLES, globalThis.MoldloftEngine)
  .forEach(function (r) {
    report(r.ok, "worked example " + r.id + ": " + r.label, r.problems);
  });

console.log(failed === 0 ? "All checks passed." : failed + " check(s) failed.");
process.exit(failed === 0 ? 0 : 1);
