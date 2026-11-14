// Pure record and code validation engine. No DOM, no storage, no network.
(function (root) {
  "use strict";

  function codePointLength(str) {
    return Array.from(str).length;
  }

  function checkCell(col, val, tableSets) {
    var n = codePointLength(val);
    var minLen = col.min_len || 0;
    var maxLen = col.max_len || 0;
    if (col.rule === "text") {
      return n >= minLen && n <= maxLen;
    }
    if (col.rule === "digits") {
      if (n < minLen || n > maxLen) {
        return false;
      }
      return /^[0-9]+$/.test(val);
    }
    if (col.rule === "upper_code") {
      if (n < minLen || n > maxLen) {
        return false;
      }
      if (col.prefix && val.indexOf(col.prefix) !== 0) {
        return false;
      }
      return /^[A-Z0-9-]+$/.test(val);
    }
    if (col.rule === "in_table") {
      return Boolean(tableSets[col.table] && tableSets[col.table][val]);
    }
    return false;
  }

  function evaluate(tool, input) {
    var tableSets = {};
    (tool.tables || []).forEach(function (tb) {
      var set = {};
      (tb.codes || []).forEach(function (code) {
        set[code] = true;
      });
      tableSets[tb.id] = set;
    });

    var normalized = String(input == null ? "" : input).replace(/\r\n/g, "\n");
    var rawLines = normalized.split("\n");
    var nonEmpty = [];
    rawLines.forEach(function (line, idx) {
      var trimmed = line.replace(/\r$/, "");
      if (trimmed.trim() === "") {
        return;
      }
      nonEmpty.push({ num: idx + 1, text: trimmed });
    });

    var faults = [];
    if (nonEmpty.length === 0) {
      faults.push({ line: 0, column: "input", code: "empty" });
      return { valid: false, row_count: 0, error_count: 1, faults: faults };
    }

    var startIdx = 0;
    if (tool.has_header) {
      var hdr = nonEmpty[0].text.split(tool.separator);
      startIdx = 1;
      if (hdr.length !== tool.columns.length) {
        faults.push({ line: nonEmpty[0].num, column: "header", code: "bad_header" });
      } else {
        tool.columns.forEach(function (col, idx) {
          if (hdr[idx].trim() !== col.id) {
            faults.push({ line: nonEmpty[0].num, column: col.id, code: "bad_header" });
          }
        });
      }
      if (nonEmpty.length === 1) {
        faults.push({ line: nonEmpty[0].num, column: "input", code: "empty" });
        return { valid: false, row_count: 0, error_count: faults.length, faults: faults };
      }
    }

    var rowCount = 0;
    nonEmpty.slice(startIdx).forEach(function (row) {
      rowCount++;
      var cells = row.text.split(tool.separator);
      if (cells.length !== tool.columns.length) {
        faults.push({ line: row.num, column: "row", code: "column_count" });
        return;
      }
      tool.columns.forEach(function (col, idx) {
        var val = cells[idx].trim();
        if (val === "") {
          if (col.required) {
            faults.push({ line: row.num, column: col.id, code: "missing" });
          }
          return;
        }
        if (!checkCell(col, val, tableSets)) {
          faults.push({ line: row.num, column: col.id, code: "invalid" });
        }
      });
    });

    return {
      valid: faults.length === 0,
      row_count: rowCount,
      error_count: faults.length,
      faults: faults
    };
  }

  root.MoldloftEngine = {
    evaluate: evaluate
  };
})(globalThis);
