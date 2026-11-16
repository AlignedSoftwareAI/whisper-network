// Every page: the ⓘ buttons of table headers (#79). Each toggles the help
// text it controls (aria-controls); Escape or a click elsewhere closes it.
// Without JavaScript the same text is the header's tooltip.
(function () {
  "use strict";
  function target(btn) {
    return document.getElementById(btn.getAttribute("aria-controls"));
  }
  function close(btn) {
    var t = target(btn);
    if (t) t.hidden = true;
    btn.setAttribute("aria-expanded", "false");
  }
  function closeAll(except) {
    document.querySelectorAll('button.colhelp[aria-expanded="true"]').forEach(function (b) {
      if (b !== except) close(b);
    });
  }
  document.addEventListener("click", function (ev) {
    var btn = ev.target.closest ? ev.target.closest("button.colhelp") : null;
    if (!btn) {
      if (!(ev.target.closest && ev.target.closest(".colhelp-text"))) closeAll(null);
      return;
    }
    ev.preventDefault();
    var t = target(btn);
    if (!t) return;
    closeAll(btn);
    var open = btn.getAttribute("aria-expanded") === "true";
    t.hidden = open;
    btn.setAttribute("aria-expanded", open ? "false" : "true");
  });
  document.addEventListener("keydown", function (ev) {
    if (ev.key === "Escape") closeAll(null);
  });
})();
