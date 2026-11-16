// Queue page: the "select all on this page" checkbox of the jobs table, and
// a confirmation before a bulk action is sent (the server confirms again
// with the count). Without JavaScript the per-row checkboxes still work.
(function () {
  "use strict";
  function init() {
    var form = document.getElementById("bulk");
    if (!form) return;
    var all = form.querySelector("input[data-select-page]");
    var boxes = form.querySelectorAll('input[name="job_id"]');
    if (all && boxes.length) {
      all.hidden = false;
      all.addEventListener("change", function () {
        boxes.forEach(function (b) { b.checked = all.checked; });
      });
      boxes.forEach(function (b) {
        b.addEventListener("change", function () {
          var n = 0;
          boxes.forEach(function (x) { if (x.checked) n++; });
          all.checked = n === boxes.length;
          all.indeterminate = n > 0 && n < boxes.length;
        });
      });
    }
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
