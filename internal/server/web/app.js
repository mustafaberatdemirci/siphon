"use strict";

// Siphon's server mode page: asks /api/state every second and draws the
// queue; buttons call the API. The token is kept in this browser only.

const $ = (id) => document.getElementById(id);
const tokenKey = "siphon-token";
let token = "";
try { token = localStorage.getItem(tokenKey) || ""; } catch (_) { /* storage blocked */ }

const last = new Map(); // job id -> {done, at, rate}
let timer = null;

async function api(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: { "Authorization": "Bearer " + token, "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (res.status === 401) {
    showLogin("The token wasn't accepted.");
    throw new Error("unauthorized");
  }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || ("HTTP " + res.status));
  return data;
}

function showLogin(message) {
  clearInterval(timer);
  timer = null;
  $("app").hidden = true;
  $("globalActions").hidden = true;
  $("login").hidden = false;
  $("loginError").hidden = !message;
  $("loginError").textContent = message || "";
  $("tokenInput").focus();
}

function start() {
  $("login").hidden = true;
  $("app").hidden = false;
  $("globalActions").hidden = false;
  refresh();
  timer = setInterval(refresh, 1000);
}

function size(n) {
  if (n < 0) return "?";
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i ? n.toFixed(1) : n) + " " + u[i];
}

function duration(s) {
  if (!isFinite(s) || s <= 0) return "";
  s = Math.round(s);
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60);
  if (h) return h + "h " + m + "m";
  if (m) return m + "m " + (s % 60) + "s";
  return s + "s";
}

const stateText = {
  queued: "Queued", running: "Downloading", paused: "Paused", done: "Complete",
  failed: "Failed", skipped: "Already downloaded", stopped: "Stopped (captcha)", waiting: "Waiting for quota",
};

// rate is the job's speed from what it downloaded since the last look,
// smoothed so the number doesn't jump about.
function rate(job, now) {
  const prev = last.get(job.id);
  let r = 0;
  if (prev && job.state === "running" && now > prev.at) {
    const inst = Math.max(0, (job.done - prev.done) / ((now - prev.at) / 1000));
    r = prev.rate ? prev.rate * 0.6 + inst * 0.4 : inst;
  }
  last.set(job.id, { done: job.done, at: now, rate: job.state === "running" ? r : 0 });
  return r;
}

function button(label, onClick) {
  const b = document.createElement("button");
  b.type = "button";
  b.textContent = label;
  b.addEventListener("click", onClick);
  return b;
}

function act(promise) {
  promise.then(refresh).catch((e) => { if (e.message !== "unauthorized") alert(e.message); });
}

// rows keeps one row per job, updated in place: a row rebuilt every second
// would swallow a click on its buttons.
const rows = new Map(); // job id -> {li, name, meta, buttons, fill, state, job}

function newRow(id) {
  const li = document.createElement("li");
  const name = document.createElement("div");
  name.className = "name";
  const buttons = document.createElement("div");
  buttons.className = "buttons";
  const meta = document.createElement("div");
  meta.className = "meta";
  const bar = document.createElement("div");
  bar.className = "progress";
  const fill = document.createElement("div");
  bar.append(fill);
  li.append(name, buttons, meta, bar);
  const row = { li, name, meta, buttons, fill, state: "", job: null };
  rows.set(id, row);
  return row;
}

function setButtons(row) {
  const job = row.job;
  const finished = job.state === "done" || job.state === "skipped";
  const list = [];
  if (job.state === "running" || job.state === "queued") {
    list.push(button("Pause", () => act(api("POST", "/api/jobs/" + row.job.id + "/pause"))));
  } else if (!finished) {
    list.push(button("Resume", () => act(api("POST", "/api/jobs/" + row.job.id + "/resume"))));
  }
  list.push(button("Remove", () => {
    const j = row.job;
    const done = j.state === "done" || j.state === "skipped";
    if (!confirm("Remove " + (j.filename || "this job") + " from the queue? Finished files are not touched.")) return;
    const del = !done && j.done > 0 && confirm("Also delete the partially downloaded file?");
    act(api("POST", "/api/jobs/" + j.id + "/remove" + (del ? "?delete=1" : "")));
  }));
  row.buttons.replaceChildren(...list);
}

function updateRow(job, now) {
  const row = rows.get(job.id) || newRow(job.id);
  row.job = job;
  row.li.className = "job " + job.state;

  const nameText = (job.dir ? job.dir + "/" : "") + (job.filename || job.source_page);
  if (row.name.dataset.text !== nameText) {
    row.name.dataset.text = nameText;
    row.name.replaceChildren();
    if (job.dir) {
      const d = document.createElement("span");
      d.className = "dir";
      d.textContent = job.dir + "/";
      row.name.append(d);
    }
    row.name.append(job.filename || job.source_page);
  }

  const r = rate(job, now);
  const parts = [];
  if (job.size > 0) parts.push(size(job.done) + " / " + size(job.size));
  else if (job.done > 0) parts.push(size(job.done));
  if (job.state === "running" && r > 0) {
    parts.push(size(r) + "/s");
    if (job.size > 0) parts.push(duration((job.size - job.done) / r) + " left");
  }
  if (job.site) parts.push(job.site);
  const st = document.createElement("span");
  st.className = "state-" + job.state;
  st.textContent = stateText[job.state] || job.state;
  row.meta.replaceChildren(st, parts.length ? " · " + parts.join(" · ") : "");
  if (job.error && job.state !== "done") row.meta.append(" — " + job.error);

  const finished = job.state === "done" || job.state === "skipped";
  const pct = finished ? 100 : job.size > 0 ? Math.min(100, (job.done / job.size) * 100) : 0;
  row.fill.style.width = pct.toFixed(1) + "%";

  if (row.state !== job.state) {
    row.state = job.state;
    setButtons(row);
  }
  return row.li;
}

// drawJobs updates the rows, drops the ones gone, and reorders only when
// the order changed (moving a row would cancel a click on it too).
function drawJobs(jobs, now) {
  const list = $("jobs");
  const seen = new Set();
  const order = jobs.map((j) => { seen.add(j.id); return updateRow(j, now); });
  for (const [id, row] of rows) {
    if (!seen.has(id)) { row.li.remove(); rows.delete(id); last.delete(id); }
  }
  const current = Array.from(list.children);
  if (current.length !== order.length || current.some((li, i) => li !== order[i])) {
    list.replaceChildren(...order);
  }
}

function summary(s) {
  const n = {};
  for (const j of s.jobs) n[j.state] = (n[j.state] || 0) + 1;
  const out = [];
  if (n.running) out.push(n.running + " downloading");
  if (n.queued) out.push(n.queued + " queued");
  if (n.waiting) out.push(n.waiting + " waiting for quota");
  if (n.paused) out.push(n.paused + " paused");
  if (n.failed) out.push(n.failed + " failed");
  const done = (n.done || 0) + (n.skipped || 0);
  if (done) out.push(done + " done");
  if (s.adding) out.push("resolving links: " + s.adding);
  return out.join(" · ");
}

let settingsShown = false;

async function refresh() {
  let s;
  try { s = await api("GET", "/api/state"); } catch (_) { return; }
  const now = Date.now();
  $("version").textContent = s.version;
  $("summary").textContent = summary(s);

  const notices = $("notices");
  notices.replaceChildren(...s.notices.slice(-5).reverse().map((n) => {
    const li = document.createElement("li");
    li.textContent = new Date(n.at).toLocaleTimeString() + "  " + n.text;
    return li;
  }));

  $("empty").hidden = s.jobs.length > 0;
  drawJobs(s.jobs, now);

  if (!settingsShown) {
    settingsShown = true;
    const sel = $("maxActive");
    for (let i = 1; i <= 8; i++) sel.append(new Option(String(i), String(i)));
    sel.value = String(s.max_active);
    $("speedLimit").value = s.speed_limit > 0 ? +(s.speed_limit / 1048576).toFixed(2) : "";
  }
}

$("loginForm").addEventListener("submit", (e) => {
  e.preventDefault();
  token = $("tokenInput").value.trim();
  try { localStorage.setItem(tokenKey, token); } catch (_) { /* storage blocked */ }
  start();
});

$("addForm").addEventListener("submit", (e) => {
  e.preventDefault();
  const links = $("links").value.split(/\s+/).filter(Boolean);
  if (!links.length) return;
  api("POST", "/api/add", { links, folder: $("folder").value })
    .then(() => { $("links").value = ""; refresh(); })
    .catch((err) => { if (err.message !== "unauthorized") alert(err.message); });
});

$("maxActive").addEventListener("change", (e) => {
  act(api("POST", "/api/settings", { max_active: Number(e.target.value) }));
});

$("speedLimit").addEventListener("change", (e) => {
  const mb = Math.max(0, Number(e.target.value) || 0);
  act(api("POST", "/api/settings", { speed_limit: Math.round(mb * 1048576) }));
});

for (const b of document.querySelectorAll("[data-all]")) {
  b.addEventListener("click", () => act(api("POST", "/api/all/" + b.dataset.all)));
}

// A link in the address after # (siphon:8080/#token=…) signs in once and
// is dropped from the address.
const m = location.hash.match(/token=([^&]+)/);
if (m) {
  token = decodeURIComponent(m[1]);
  try { localStorage.setItem(tokenKey, token); } catch (_) { /* storage blocked */ }
  history.replaceState(null, "", location.pathname);
}

if (token) start(); else showLogin("");
