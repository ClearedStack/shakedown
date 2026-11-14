// Connects the validator HTML form to MoldloftEngine. Holds input in browser
// memory only; reads nothing from and writes nothing to browser storage.
(function () {
  "use strict";

  var tool = globalThis.MOLDLOFT_TOOL;
  var engine = globalThis.MoldloftEngine;
  var BLANK = "-";

  function el(id) {
    return document.getElementById(id);
  }

  function faultMessage(fault) {
    var where = fault.line > 0 ? "Line " + fault.line : "Input";
    if (fault.code === "empty") {
      return where + ": no data rows found.";
    }
    if (fault.code === "bad_header") {
      return where + " (" + fault.column + "): header does not match expected column names.";
    }
    if (fault.code === "column_count") {
      return where + ": wrong number of delimited columns.";
    }
    if (fault.code === "missing") {
      return where + " (" + fault.column + "): required value is missing.";
    }
    return where + " (" + fault.column + "): value does not satisfy column rule.";
  }

  function renderFindings(faults) {
    var list = el("findings");
    while (list.firstChild) {
      list.removeChild(list.firstChild);
    }
    if (faults.length === 0) {
      var okItem = document.createElement("li");
      okItem.className = "pass";
      okItem.textContent = "No validation errors found.";
      list.appendChild(okItem);
      return;
    }
    faults.forEach(function (f) {
      var li = document.createElement("li");
      li.className = "fail";
      li.textContent = faultMessage(f);
      list.appendChild(li);
    });
  }

  function recompute() {
    var raw = el("in-text").value;
    if (raw.trim() === "") {
      el("out-status").textContent = BLANK;
      el("out-rows").textContent = BLANK;
      el("out-errors").textContent = BLANK;
      el("status").textContent = "Paste records or load a local text file to validate.";
      var list = el("findings");
      while (list.firstChild) {
        list.removeChild(list.firstChild);
      }
      return;
    }
    var result = engine.evaluate(tool, raw);
    el("out-status").textContent = result.valid ? "Valid" : "Invalid";
    el("out-rows").textContent = String(result.row_count);
    el("out-errors").textContent = String(result.error_count);
    el("status").textContent = result.valid
      ? "All rows satisfy the published column rules."
      : result.error_count + " validation finding(s) across " + result.row_count + " row(s).";
    renderFindings(result.faults);
  }

  function wire() {
    var form = el("inputs");
    var text = el("in-text");
    var file = el("in-file");
    form.addEventListener("submit", function (event) {
      event.preventDefault();
    });
    form.addEventListener("reset", function () {
      setTimeout(function () {
        text.value = "";
        recompute();
      }, 0);
    });
    text.addEventListener("input", recompute);
    text.addEventListener("change", recompute);
    file.addEventListener("change", function () {
      if (!file.files || file.files.length === 0) {
        return;
      }
      var reader = new FileReader();
      reader.addEventListener("load", function () {
        text.value = typeof reader.result === "string" ? reader.result : "";
        recompute();
      });
      reader.readAsText(file.files[0]);
    });
    recompute();
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", wire);
  } else {
    wire();
  }
})();
