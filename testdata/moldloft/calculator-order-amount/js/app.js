// Connects the page to the engine: reads the inputs, works the steps out and
// writes the results back. Nothing is stored and nothing is sent anywhere.
(function (root) {
  "use strict";

  var tool = root.MOLDLOFT_TOOL;
  var engine = root.MoldloftEngine;
  var doc = root.document;

  var MESSAGES = {
    missing: "Enter a value.",
    format: "Enter a plain number, such as 12 or 12.5, with no commas or symbols.",
    decimals: "This value has too many decimal places.",
    range: "This value is outside the range allowed.",
    choice: "Choose one of the listed options.",
    divide_by_zero: "Cannot be worked out: this step would divide by zero.",
    overflow: "Cannot be worked out: the result is too large."
  };
  var BLANK = "-";

  function el(id) {
    return doc.getElementById(id);
  }

  function update() {
    var raw = {};
    tool.inputs.forEach(function (input) {
      raw[input.id] = el("in-" + input.id).value.trim();
    });
    var result = engine.evaluate(tool, raw);
    var fault = {};
    result.faults.forEach(function (f) {
      fault[f.id] = MESSAGES[f.kind];
    });

    tool.inputs.forEach(function (input) {
      var field = el("in-" + input.id);
      var note = el("err-" + input.id);
      if (fault[input.id]) {
        field.setAttribute("aria-invalid", "true");
        note.textContent = fault[input.id];
      } else {
        field.removeAttribute("aria-invalid");
        note.textContent = "";
      }
    });
    tool.steps.forEach(function (step) {
      var cell = el("out-" + step.id);
      var has = Object.prototype.hasOwnProperty.call(result.values, step.id);
      cell.textContent = has ? result.values[step.id] : (fault[step.id] || BLANK);
      var total = el("total-" + step.id);
      if (total) {
        total.textContent = has ? result.values[step.id] : BLANK;
      }
    });
    el("status").textContent = result.faults.length === 0
      ? "Worked out from the values above."
      : "Some results cannot be shown. Check the marked values.";
  }

  function start() {
    var form = el("inputs");
    form.addEventListener("submit", function (event) {
      event.preventDefault();
    });
    form.addEventListener("input", update);
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
