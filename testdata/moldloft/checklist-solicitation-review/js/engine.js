// Deterministic checklist evaluator shared by the page and the worked-example
// checks. No network, no storage, no clock.
(function (root) {
  "use strict";

  function evaluate(tool, answers) {
    var yesCount = 0;
    var noCount = 0;
    var naCount = 0;
    var unmetRequired = 0;
    var faults = [];

    tool.items.forEach(function (item) {
      var ans = Object.prototype.hasOwnProperty.call(answers, item.id) ? answers[item.id] : "";
      if (!ans) {
        faults.push({ id: item.id, kind: "missing" });
        return;
      }
      if (ans === "yes") {
        yesCount++;
      } else if (ans === "no") {
        noCount++;
        if (item.required) {
          unmetRequired++;
        }
      } else if (ans === "na") {
        if (!item.na_allowed) {
          faults.push({ id: item.id, kind: "choice" });
          return;
        }
        naCount++;
      } else {
        faults.push({ id: item.id, kind: "choice" });
      }
    });

    var status = "pending";
    if (faults.length === 0) {
      status = unmetRequired === 0 ? "complete" : "incomplete";
    }
    return {
      status: status,
      unmet_required: unmetRequired,
      yes_count: yesCount,
      no_count: noCount,
      na_count: naCount,
      faults: faults
    };
  }

  root.MoldloftEngine = { evaluate: evaluate };
})(globalThis);
