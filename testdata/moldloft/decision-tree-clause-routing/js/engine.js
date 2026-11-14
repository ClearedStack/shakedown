// Pure branching decision-tree engine. No DOM, no storage, no network.
(function (root) {
  "use strict";

  function evaluate(tool, answers) {
    var questions = {};
    (tool.questions || []).forEach(function (q) {
      questions[q.id] = q;
    });
    var outcomes = {};
    (tool.outcomes || []).forEach(function (out) {
      outcomes[out.id] = out;
    });

    var given = answers || {};
    var path = [];
    var faults = [];
    var seen = {};
    var cur = tool.start;

    while (true) {
      if (seen[cur]) {
        faults.push({ question: cur, kind: "cycle" });
        return { complete: false, current_question: cur, steps: path.length, path: path, faults: faults };
      }
      seen[cur] = true;
      var q = questions[cur];
      if (!q) {
        faults.push({ question: cur, kind: "unknown_question" });
        return { complete: false, current_question: cur, steps: path.length, path: path, faults: faults };
      }
      var ans = String(given[cur] == null ? "" : given[cur]).trim();
      if (ans === "") {
        return {
          complete: false,
          current_question: cur,
          steps: path.length,
          path: path,
          faults: faults
        };
      }
      var matched = null;
      for (var i = 0; i < q.options.length; i++) {
        if (q.options[i].key === ans) {
          matched = q.options[i];
          break;
        }
      }
      if (!matched) {
        faults.push({ question: cur, kind: "invalid_option" });
        return {
          complete: false,
          current_question: cur,
          steps: path.length,
          path: path,
          faults: faults
        };
      }
      path.push({
        question: q.id,
        option: matched.key,
        label: matched.label
      });
      var out = outcomes[matched.next];
      if (out) {
        return {
          complete: true,
          outcome: out.id,
          title: out.title,
          summary: out.summary,
          status: out.status,
          citation: out.citation,
          steps: path.length,
          path: path,
          faults: faults
        };
      }
      cur = matched.next;
    }
  }

  root.MoldloftEngine = {
    evaluate: evaluate
  };
})(globalThis);
