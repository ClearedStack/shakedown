// Shows the worked-example results on tests/index.html.
(function (root) {
  "use strict";

  var doc = root.document;
  var results = root.MoldloftCheck.run(root.MOLDLOFT_TOOL, root.MOLDLOFT_EXAMPLES, root.MoldloftEngine);
  var list = doc.getElementById("results");
  var failed = 0;

  results.forEach(function (r) {
    var item = doc.createElement("li");
    item.className = r.ok ? "pass" : "fail";
    item.textContent = (r.ok ? "Pass: " : "Fail: ") + r.label;
    r.problems.forEach(function (p) {
      var line = doc.createElement("div");
      line.textContent = p;
      item.appendChild(line);
    });
    list.appendChild(item);
    if (!r.ok) {
      failed++;
    }
  });

  var summary = doc.getElementById("summary");
  summary.className = failed === 0 ? "pass" : "fail";
  summary.textContent = failed === 0
    ? "All " + results.length + " worked examples pass."
    : failed + " of " + results.length + " worked examples fail.";
})(globalThis);
