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

// ---- disk image jobs

function inventory() {
  var el = document.getElementById("inventory");
  if (!el) return {};
  try { return JSON.parse(el.textContent) || {}; } catch (e) { return {}; }
}

function fmtBytes(b) {
  var u = ["B", "KiB", "MiB", "GiB", "TiB"], i = 0;
  while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; }
  return (i ? b.toFixed(1) : b) + " " + u[i];
}

var partKinds = {
  "C12A7328-F81F-11D2-BA4B-00A0C93EC93B": "EFI System",
  "E3C9E316-0B5C-4DB8-817D-F92DF00215AE": "Microsoft Reserved",
  "EBD0A0A2-B9E5-4433-87C0-68B6B72699C7": "Basic data",
  "DE94BBA4-06D1-4D40-A16A-BFD50179D6AC": "Recovery"
};

function partText(p) {
  var parts = [p.number + ".", fmtBytes(p.length)];
  if (p.mount_points) parts.push(p.mount_points.map(function (m) { return m.replace(/\\+$/, ""); }).join(" "));
  if (p.label) parts.push("“" + p.label + "”");
  if (p.file_system) parts.push(p.file_system);
  var kind = partKinds[(p.gpt_type || "").toUpperCase()];
  if (kind) parts.push("(" + kind + ")");
  return parts.join("  ");
}

function el(tag, attrs, text) {
  var e = document.createElement(tag);
  for (var k in attrs || {}) e.setAttribute(k, attrs[k]);
  if (text) e.textContent = text;
  return e;
}

function syncJobKind() {
  var sel = document.querySelector("input[name=kind]:checked");
  if (!sel) return;
  document.querySelectorAll("[data-jobkind]").forEach(function (box) {
    var on = box.getAttribute("data-jobkind") === sel.value;
    box.hidden = !on;
    box.querySelectorAll("input,textarea,select").forEach(function (i) { i.disabled = !on; });
  });
  document.querySelectorAll("[data-notkind]").forEach(function (el) {
    var off = el.getAttribute("data-notkind") === sel.value;
    el.hidden = off;
    el.querySelectorAll("select,input").forEach(function (i) { i.disabled = off; });
  });
  document.querySelectorAll("[data-jobkind-opt]").forEach(function (o) {
    o.hidden = o.disabled = o.getAttribute("data-jobkind-opt") !== sel.value;
    if (o.disabled && o.selected) { o.parentNode.value = "daily"; syncSched(); }
  });
  var after = document.querySelector("option[value=after]");
  if (sel.value === "copy" && after && !after.dataset.touched) { after.dataset.touched = "1"; after.parentNode.value = "after"; syncSched(); }
  if (sel.value === "image") syncDiskChoice();
}

function renderDiskPicker() {
  var box = document.getElementById("disk-picker");
  var agentSel = document.getElementById("agent_sel");
  if (!box || !agentSel) return;
  box.textContent = "";
  var disks = inventory()[agentSel.value];
  if (!disks || !disks.length) {
    box.appendChild(el("p", { "class": "muted" }, "This agent has not reported its disks yet. Windows agents report them within a few minutes of starting."));
    return;
  }
  var chosen = disks.filter(function (d) { return d.system; })[0] || disks[0];
  disks.forEach(function (d) {
    var wrap = el("div", { "class": "disk" });
    var head = el("label", { "class": "check" });
    var radio = el("input", { type: "radio", name: "image_disk", value: d.number });
    if (d === chosen) radio.checked = true;
    head.appendChild(radio);
    head.appendChild(document.createTextNode(" Disk " + d.number + " — " + (d.model || "disk") + ", " + fmtBytes(d.size) + ", " + d.style.toUpperCase() + (d.system ? "  (system disk)" : "")));
    wrap.appendChild(head);
    var parts = el("div", { "class": "parts" });
    (d.partitions || []).forEach(function (p) {
      var l = el("label", { "class": "check" });
      var cb = el("input", { type: "checkbox", name: "image_parts", value: p.number, "data-disk": d.number });
      cb.checked = true;
      l.appendChild(cb);
      l.appendChild(document.createTextNode(" " + partText(p)));
      parts.appendChild(l);
    });
    wrap.appendChild(parts);
    box.appendChild(wrap);
  });
  syncDiskChoice();
}

// Only partitions of the selected disk are submitted.
function syncDiskChoice() {
  var r = document.querySelector("input[name=image_disk]:checked");
  document.querySelectorAll("input[name=image_parts]").forEach(function (cb) {
    var on = r && cb.getAttribute("data-disk") === r.value;
    cb.disabled = !on;
    cb.parentNode.classList.toggle("muted", !on);
  });
}

function renderRestoreDisks() {
  var agentSel = document.getElementById("restore_agent");
  var diskSel = document.getElementById("restore_disk");
  if (!agentSel || !diskSel) return;
  diskSel.textContent = "";
  var disks = (inventory()[agentSel.value] || []).filter(function (d) { return !d.system; });
  if (!disks.length) {
    var o = el("option", { value: "" }, "No other disks reported by this agent");
    diskSel.appendChild(o);
    return;
  }
  disks.forEach(function (d) {
    var used = (d.partitions || []).length ? (d.partitions.length + " partitions") : "empty";
    diskSel.appendChild(el("option", { value: d.number }, "Disk " + d.number + " — " + (d.model || "disk") + ", " + fmtBytes(d.size) + ", " + used));
  });
}

// ---- Proxmox VM jobs

function renderVMPicker() {
  var box = document.getElementById("vm-picker");
  var agentSel = document.getElementById("agent_sel");
  var data = document.getElementById("pve-inventory");
  if (!box || !agentSel || !data) return;
  var inv = {};
  try { inv = JSON.parse(data.textContent) || {}; } catch (e) {}
  box.textContent = "";
  var guests = inv[agentSel.value];
  if (!guests) {
    box.appendChild(el("p", { "class": "muted" }, "This agent is not on a Proxmox VE or Hyper-V host. Select the agent installed on the hypervisor host."));
    return;
  }
  if (!guests.length) {
    box.appendChild(el("p", { "class": "muted" }, "No VMs or containers on this node yet."));
    return;
  }
  guests.forEach(function (g) {
    var l = el("label", { "class": "check" });
    var cb = el("input", { type: "checkbox", name: "vms", value: g.vmid });
    l.appendChild(cb);
    var kind = g.type === "lxc" ? "container" : "VM";
    var label = g.id ? (g.name || g.id) : g.vmid + "  " + (g.name || "");
    l.appendChild(document.createTextNode(" " + label + "  (" + kind + ", " + fmtBytes(g.maxdisk || 0) + ", " + g.status + ")"));
    box.appendChild(l);
  });
}

document.addEventListener("change", function (e) {
  if (e.target.hasAttribute && e.target.hasAttribute("data-kindsel")) syncJobKind();
  if (e.target.id === "agent_sel") renderDiskPicker();
  if (e.target.id === "agent_sel") renderVMPicker();
  if (e.target.name === "vms") { var s = document.querySelector("input[name=vm_mode][value=selected]"); if (s) s.checked = true; }
  if (e.target.name === "image_disk") syncDiskChoice();
  if (e.target.id === "restore_agent") renderRestoreDisks();
});
document.addEventListener("DOMContentLoaded", function () {
  renderDiskPicker();
  renderVMPicker();
  syncJobKind();
  renderRestoreDisks();
});

// "Select all" checkbox: <input type="checkbox" data-checkall="name">
document.addEventListener("change", function (e) {
  var name = e.target.getAttribute && e.target.getAttribute("data-checkall");
  if (!name) return;
  document.querySelectorAll('input[name="' + name + '"]').forEach(function (c) { c.checked = e.target.checked; });
});

// Sidebar toggle on small screens: <button data-toggle="#side">
document.addEventListener("click", function (e) {
  var btn = e.target.closest && e.target.closest("[data-toggle]");
  if (btn) {
    var el = document.querySelector(btn.getAttribute("data-toggle"));
    if (el) el.classList.toggle("open");
    return;
  }
  var side = document.getElementById("side");
  if (side && side.classList.contains("open") && !side.contains(e.target)) side.classList.remove("open");
});

// Print buttons: <button data-print>
document.addEventListener("click", function (e) {
  if (e.target.closest && e.target.closest("[data-print]")) window.print();
});

// Selects that submit their form on change: <select data-autosubmit>
document.addEventListener("change", function (e) {
  if (e.target.hasAttribute && e.target.hasAttribute("data-autosubmit")) e.target.form.submit();
});

// Report period: show the date fields only for a custom range.
function syncPeriod() {
  var sel = document.getElementById("period");
  if (!sel) return;
  document.querySelectorAll("[data-custom]").forEach(function (el) {
    el.hidden = sel.value !== "custom";
    el.querySelectorAll("input").forEach(function (i) { i.disabled = el.hidden; });
  });
}
document.addEventListener("change", function (e) { if (e.target.id === "period") syncPeriod(); });
document.addEventListener("DOMContentLoaded", syncPeriod);

// Calendar: "+N more" shows all entries of a day; click again to collapse.
document.addEventListener("click", function (e) {
  var btn = e.target.closest && e.target.closest("[data-expand]");
  if (!btn) return;
  var day = btn.closest(".calday");
  var open = day.classList.toggle("expanded");
  btn.setAttribute("aria-expanded", open ? "true" : "false");
  if (!btn.dataset.label) btn.dataset.label = btn.textContent;
  btn.textContent = open ? "show less" : btn.dataset.label;
});

// Filter bars: text search and kind chips over the rows of a table.
document.addEventListener("DOMContentLoaded", function () { document.querySelectorAll("[data-filter-table]").forEach(function (bar) {
  var table = document.getElementById(bar.getAttribute("data-filter-table"));
  if (!table) return;
  var input = bar.querySelector("[data-filter-text]");
  var kind = "";
  var empty = bar.parentNode.querySelector("[data-filter-empty]");
  function apply() {
    var q = (input && input.value || "").trim().toLowerCase();
    var shown = 0;
    table.querySelectorAll("tr[data-kind]").forEach(function (tr) {
      var kinds = tr.getAttribute("data-kind").split(" ");
      var ok = (!kind || kinds.indexOf(kind) >= 0) && (!q || tr.textContent.toLowerCase().indexOf(q) >= 0);
      tr.hidden = !ok;
      if (ok) shown++;
    });
    if (empty) empty.hidden = shown > 0;
  }
  bar.querySelectorAll("[data-filter-kind]").forEach(function (b) {
    b.addEventListener("click", function () {
      kind = b.getAttribute("data-filter-kind");
      bar.querySelectorAll("[data-filter-kind]").forEach(function (x) { x.classList.toggle("on", x === b); });
      apply();
    });
  });
  if (input) input.addEventListener("input", apply);
}); });
