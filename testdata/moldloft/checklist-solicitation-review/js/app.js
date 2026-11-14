// Connects the checklist form to the engine in browser memory. Nothing is
// stored outside page memory and nothing is sent anywhere.
(function (root) {
  "use strict";

  var tool = root.MOLDLOFT_TOOL;
  var engine = root.MoldloftEngine;
  var doc = root.document;

  function el(id) {
    return doc.getElementById(id);
  }

  function update() {
    var answers = {};
    tool.items.forEach(function (item) {
      answers[item.id] = el("in-" + item.id).value;
    });
    var result = engine.evaluate(tool, answers);
    var faultMap = {};
    result.faults.forEach(function (f) {
      faultMap[f.id] = f.kind;
    });

    tool.items.forEach(function (item) {
      var field = el("in-" + item.id);
      var note = el("err-" + item.id);
      if (faultMap[item.id] === "missing") {
        field.setAttribute("aria-invalid", "true");
        note.textContent = "Select an answer.";
      } else if (faultMap[item.id]) {
        field.setAttribute("aria-invalid", "true");
        note.textContent = "Choose one of the listed options.";
      } else {
        field.removeAttribute("aria-invalid");
        note.textContent = "";
      }
    });

    el("out-status").textContent = result.status;
    el("out-unmet").textContent = result.faults.length === 0 ? String(result.unmet_required) : "-";
    el("out-yes").textContent = result.faults.length === 0 ? String(result.yes_count) : "-";
    el("out-no").textContent = result.faults.length === 0 ? String(result.no_count) : "-";
    el("out-na").textContent = result.faults.length === 0 ? String(result.na_count) : "-";
    el("status").textContent = result.faults.length === 0
      ? "Checklist evaluated from the answers above."
      : "Answer every item to see the final checklist status.";
  }

  function start() {
    var form = el("inputs");
    form.addEventListener("submit", function (event) {
      event.preventDefault();
    });
    form.addEventListener("change", update);
    form.addEventListener("reset", function () {
      root.setTimeout(update, 0);
    });
    update();
  }

  if (doc.readyState === "loading") {
    doc.addEventListener("DOMContentLoaded", start);
  } else {
    start();
  }
})(globalThis);
