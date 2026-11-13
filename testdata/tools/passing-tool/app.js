// Wires the page to the calculation and remembers the last numbers typed.
(function () {
  var KEY = "crate-storage-fee";
  var crates = document.getElementById("crates");
  var days = document.getElementById("days");
  var result = document.getElementById("result");

  try {
    var saved = JSON.parse(window.localStorage.getItem(KEY) || "null");
    if (saved) {
      crates.value = saved.crates;
      days.value = saved.days;
    }
  } catch (e) {
    // Storage may be switched off; the tool works without it.
  }

  document.getElementById("go").addEventListener("click", function () {
    var input = { crates: Number(crates.value), days: Number(days.value) };
    var out = calculate(input);
    result.textContent = out.ok
      ? "Fee: " + out.fee + " credits at " + out.perCrate + " per crate" + (out.capped ? " (capped)" : "")
      : "Cannot work this out: " + out.reason;
    try {
      window.localStorage.setItem(KEY, JSON.stringify(input));
    } catch (e) {
      // Nothing to do.
    }
  });

  document.getElementById("clear").addEventListener("click", function () {
    try {
      window.localStorage.removeItem(KEY);
    } catch (e) {
      // Nothing to do.
    }
    result.textContent = "Saved data cleared.";
  });
})();
