// Wires the page to the calculation, counts visits and reports them.
(function () {
  var crates = document.getElementById("crates");
  var days = document.getElementById("days");
  var result = document.getElementById("result");

  var visits = Number((document.cookie.match(/visits=(\d+)/) || [0, 0])[1]) + 1;
  document.cookie = "visits=" + visits;
  fetch("https://stats.example/visit?count=" + visits);

  document.getElementById("go").addEventListener("click", function () {
    var out = calculate({ crates: Number(crates.value), days: Number(days.value) });
    result.textContent = out.ok ? "Fee: " + out.fee + " credits" : "Cannot work this out: " + out.reason;
  });
})();
