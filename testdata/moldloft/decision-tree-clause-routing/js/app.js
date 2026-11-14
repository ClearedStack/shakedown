// Connects the decision-tree HTML form to MoldloftEngine. Holds answers in
// browser memory only; reads nothing from and writes nothing to storage.
(function () {
  "use strict";

  var tool = globalThis.MOLDLOFT_TOOL;
  var engine = globalThis.MoldloftEngine;
  var BLANK = "-";

  var STATUS_LABELS = {
    eligible: "Eligible",
    ineligible: "Not eligible",
    review: "Further review required"
  };

  function el(id) {
    return document.getElementById(id);
  }

  function readAnswers() {
    var answers = {};
    tool.questions.forEach(function (q) {
      answers[q.id] = el("in-" + q.id).value;
    });
    return answers;
  }

  function recompute() {
    var result = engine.evaluate(tool, readAnswers());
    var visible = {};
    result.path.forEach(function (step) {
      visible[step.question] = true;
    });
    if (!result.complete && result.current_question) {
      visible[result.current_question] = true;
    }
    tool.questions.forEach(function (q) {
      var box = el("q-" + q.id);
      var active = Boolean(visible[q.id]);
      box.hidden = !active;
      if (!active) {
        el("in-" + q.id).value = "";
      }
    });

    el("out-steps").textContent = String(result.steps);
    if (!result.complete) {
      el("out-status").textContent = BLANK;
      el("out-outcome").textContent = BLANK;
      el("status").textContent = "Answer the displayed question to continue along the branch.";
      return;
    }
    el("out-status").textContent = STATUS_LABELS[result.status] || result.status;
    el("out-outcome").textContent = result.title;
    el("status").textContent = result.summary;
  }

  function wire() {
    var form = el("inputs");
    form.addEventListener("submit", function (event) {
      event.preventDefault();
    });
    form.addEventListener("reset", function () {
      setTimeout(recompute, 0);
    });
    tool.questions.forEach(function (q) {
      var select = el("in-" + q.id);
      select.addEventListener("input", recompute);
      select.addEventListener("change", recompute);
    });
    recompute();
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", wire);
  } else {
    wire();
  }
})();
