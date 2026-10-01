// Confirmation for destructive actions: <form data-confirm="Really?">
document.addEventListener("submit", function (e) {
  var msg = e.target.getAttribute("data-confirm");
  if (msg && !window.confirm(msg)) e.preventDefault();
});

// Show only the fields of the selected storage type.
function syncKind() {
  var sel = document.getElementById("kind");
  if (!sel) return;
  document.querySelectorAll("[data-kind]").forEach(function (el) {
    el.hidden = el.getAttribute("data-kind") !== sel.value;
    el.querySelectorAll("input,textarea,select").forEach(function (i) { i.disabled = el.hidden; });
  });
}
document.addEventListener("change", function (e) {
  if (e.target.id === "kind") syncKind();
});
document.addEventListener("DOMContentLoaded", syncKind);

// Copy buttons: <button data-copy="#id">
document.addEventListener("click", function (e) {
  var sel = e.target.getAttribute && e.target.getAttribute("data-copy");
  if (!sel) return;
  var el = document.querySelector(sel);
  if (el && navigator.clipboard) {
    navigator.clipboard.writeText(el.textContent.trim());
    e.target.textContent = "Copied";
  }
});

// Auto-refresh pages with active runs.
document.addEventListener("DOMContentLoaded", function () {
  if (document.querySelector("[data-autorefresh]")) {
    setTimeout(function () { location.reload(); }, 5000);
  }
});

// Schedule form: show the fields of the selected schedule type.
// Elements list the types they belong to in data-sched="daily interval".
function syncSched() {
  var sel = document.getElementById("sched_kind");
  if (!sel) return;
  document.querySelectorAll("[data-sched]").forEach(function (el) {
    var on = el.getAttribute("data-sched").split(" ").indexOf(sel.value) >= 0;
    el.hidden = !on;
    el.querySelectorAll("input,select").forEach(function (i) { i.disabled = !on; });
  });
}
document.addEventListener("change", function (e) {
  if (e.target.id === "sched_kind") syncSched();
});
document.addEventListener("DOMContentLoaded", syncSched);
