"use strict";

// --- token handling -------------------------------------------------------
// The gateway token authenticates every fetch below via the Authorization
// header. On first load it may arrive as ?token=... (server.go accepts that
// only for GET /dashboard*) — stash it and strip it from the visible URL
// immediately so it doesn't linger in browser history longer than needed.
const TOKEN_KEY = "heka_dashboard_token";

function initToken() {
  const url = new URL(window.location.href);
  const fromQuery = url.searchParams.get("token");
  if (fromQuery) {
    sessionStorage.setItem(TOKEN_KEY, fromQuery);
    url.searchParams.delete("token");
    history.replaceState(null, "", url.pathname + url.search + url.hash);
  }
}
initToken();

function getToken() {
  return sessionStorage.getItem(TOKEN_KEY) || "";
}

function setToken(t) {
  sessionStorage.setItem(TOKEN_KEY, t);
}

async function api(path, opts) {
  opts = opts || {};
  const headers = Object.assign({}, opts.headers || {});
  const token = getToken();
  if (token) headers["Authorization"] = "Bearer " + token;
  if (opts.body && !headers["Content-Type"]) headers["Content-Type"] = "application/json";
  const res = await fetch(path, Object.assign({}, opts, { headers }));
  if (res.status === 401) {
    sessionStorage.removeItem(TOKEN_KEY);
    showTokenPrompt();
    throw new Error("unauthorized");
  }
  return res;
}

async function apiJSON(path, opts) {
  const res = await api(path, opts);
  const data = await res.json().catch(() => null);
  if (!res.ok) {
    const msg = (data && data.error && data.error.message) || res.statusText;
    const err = new Error(msg);
    err.status = res.status;
    err.data = data;
    throw err;
  }
  return data;
}

function showTokenPrompt() {
  document.getElementById("token-prompt").classList.remove("hidden");
  document.getElementById("auth-status").textContent = "not authenticated";
}

function hideTokenPrompt() {
  document.getElementById("token-prompt").classList.add("hidden");
  document.getElementById("auth-status").textContent = "";
}

document.getElementById("token-submit").addEventListener("click", () => {
  const v = document.getElementById("token-input").value.trim();
  if (!v) return;
  setToken(v);
  hideTokenPrompt();
  refreshActiveTab();
});

// --- tabs ------------------------------------------------------------------
const tabs = ["overview", "requests", "config"];
function activateTab(name) {
  for (const t of tabs) {
    document.getElementById("tab-" + t).classList.toggle("active", t === name);
  }
  document.querySelectorAll("#tabs button").forEach((b) => {
    b.classList.toggle("active", b.dataset.tab === name);
  });
  refreshActiveTab();
}
document.querySelectorAll("#tabs button").forEach((b) => {
  b.addEventListener("click", () => activateTab(b.dataset.tab));
});

function activeTabName() {
  return document.querySelector("#tabs button.active").dataset.tab;
}

function refreshActiveTab() {
  const name = activeTabName();
  if (name === "overview") loadOverview();
  if (name === "requests") loadRequests();
  if (name === "config") loadConfig();
}

// --- formatting helpers -----------------------------------------------------
function statusClass(status) {
  if (!status) return "status-err";
  if (status < 300) return "status-2xx";
  if (status < 400) return "status-3xx";
  if (status < 500) return "status-4xx";
  return "status-5xx";
}

function el(tag, attrs, children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (k === "class") e.className = v;
    else e.setAttribute(k, v);
  }
  for (const c of children || []) {
    e.appendChild(typeof c === "string" ? document.createTextNode(c) : c);
  }
  return e;
}

function clear(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
}

function fmtTime(iso) {
  const d = new Date(iso);
  return d.toLocaleTimeString();
}

// --- overview ----------------------------------------------------------------
async function loadOverview() {
  let ov;
  try {
    ov = await apiJSON("/dashboard/api/overview");
  } catch (e) {
    return;
  }
  const tiles = document.getElementById("stat-tiles");
  clear(tiles);
  const w = ov.window || {};
  const items = [
    ["Total requests", ov.total],
    ["Error rate (5m)", w.count ? Math.round(w.error_rate * 1000) / 10 + "%" : "–"],
    ["p95 latency (5m)", w.p95_ms != null ? w.p95_ms + "ms" : "–"],
    ["Uptime", Math.round((ov.uptime_s || 0) / 1e9 / 60) + "m"],
    ["History used", (ov.history_used || 0) + "/" + (ov.history_size || 0)],
  ];
  for (const [label, value] of items) {
    tiles.appendChild(
      el("div", { class: "tile" }, [
        el("div", { class: "value" }, [String(value)]),
        el("div", { class: "label" }, [label]),
      ])
    );
  }

  let status;
  try {
    status = await apiJSON("/dashboard/api/status");
  } catch (e) {
    status = null;
  }
  const proxies = document.getElementById("proxies");
  clear(proxies);
  if (status) {
    for (const [name, keys] of Object.entries(status.providers || {})) {
      const block = el("div", { class: "proxy-block" }, [el("h3", {}, [name])]);
      const table = el("table", {}, []);
      const tbody = el("tbody", {}, []);
      for (const k of keys) {
        tbody.appendChild(
          el("tr", {}, [
            el("td", {}, [k.key]),
            el("td", {}, [el("span", { class: "pill " + k.state }, [k.state])]),
            el("td", {}, [String(k.successes) + " ok"]),
            el("td", {}, [String(k.failures) + " fail"]),
          ])
        );
      }
      table.appendChild(tbody);
      block.appendChild(table);
      proxies.appendChild(block);
    }
    if (status.sidecars) {
      const block = el("div", { class: "proxy-block" }, [el("h3", {}, ["sidecars"])]);
      for (const [name, state] of Object.entries(status.sidecars)) {
        block.appendChild(el("span", { class: "pill " + state }, [name + ": " + state]));
        block.appendChild(document.createTextNode(" "));
      }
      proxies.appendChild(block);
    }
  }

  let errs;
  try {
    errs = await apiJSON("/dashboard/api/errors?limit=20");
  } catch (e) {
    errs = [];
  }
  const errBox = document.getElementById("errors");
  clear(errBox);
  if (!errs || errs.length === 0) {
    errBox.appendChild(el("div", { class: "detail" }, ["no recent errors"]));
    return;
  }
  const table = el("table", {}, []);
  const tbody = el("tbody", {}, []);
  for (const e of errs) {
    tbody.appendChild(
      el("tr", {}, [
        el("td", {}, [fmtTime(e.time)]),
        el("td", { class: "status-err" }, [e.level]),
        el("td", {}, [e.msg]),
      ])
    );
  }
  table.appendChild(tbody);
  errBox.appendChild(table);
}

// --- requests ------------------------------------------------------------------
let selectedRequestId = null;

async function loadRequests() {
  const onlyErrors = document.getElementById("only-errors").checked;
  let list;
  try {
    list = await apiJSON("/dashboard/api/requests?limit=100" + (onlyErrors ? "&errors=1" : ""));
  } catch (e) {
    return;
  }
  const tbody = document.querySelector("#requests-table tbody");
  clear(tbody);
  for (const r of list || []) {
    const tr = el("tr", { "data-id": String(r.id) }, [
      el("td", {}, [fmtTime(r.time)]),
      el("td", {}, [r.route || ""]),
      el("td", {}, [r.method || ""]),
      el("td", {}, [r.path || ""]),
      el("td", { class: statusClass(r.status) }, [String(r.status)]),
      el("td", {}, [String(r.attempts)]),
      el("td", {}, [String(r.duration_ms)]),
    ]);
    tr.addEventListener("click", () => showRequestDetail(r.id));
    tbody.appendChild(tr);
  }
}

async function showRequestDetail(id) {
  selectedRequestId = id;
  const pre = document.getElementById("request-detail");
  pre.textContent = "loading…";
  let rec;
  try {
    rec = await apiJSON("/dashboard/api/requests/" + id);
  } catch (e) {
    pre.textContent = "failed to load: " + e.message;
    return;
  }
  const lines = [];
  lines.push(rec.time + "  " + rec.method + " " + rec.path + (rec.query ? "?" + rec.query : ""));
  lines.push("route=" + rec.route + " kind=" + rec.kind + " status=" + rec.status +
    " attempts=" + rec.attempts + " duration=" + rec.duration_ms + "ms");
  if (rec.key) lines.push("key=" + rec.key);
  if (rec.verdict) lines.push("verdict=" + rec.verdict);
  if (rec.error) lines.push("error=" + rec.error);
  if (rec.streaming) lines.push("(streaming response)");
  if (rec.capture_skipped) lines.push("response not captured: " + rec.capture_skipped);
  if (rec.body_evicted) lines.push("(bodies evicted to stay under the memory budget)");
  lines.push("");
  lines.push("--- request body ---");
  lines.push(formatBody(rec.req_body));
  lines.push("");
  lines.push("--- response body ---");
  lines.push(formatBody(rec.resp_body));
  pre.textContent = lines.join("\n");
}

function formatBody(b) {
  if (!b) return "(not captured)";
  let text = b.text;
  if (b.encoding === "utf8") {
    try {
      text = JSON.stringify(JSON.parse(text), null, 2);
    } catch (e) {
      // not JSON, show as-is
    }
  } else {
    text = "(binary, base64) " + text;
  }
  if (b.truncated) text += "\n… (truncated)";
  return text;
}

document.getElementById("only-errors").addEventListener("change", loadRequests);

// --- config ------------------------------------------------------------------
let configVersion = "";

async function loadConfig() {
  let cfg;
  try {
    cfg = await apiJSON("/dashboard/api/config");
  } catch (e) {
    return;
  }
  document.getElementById("config-path").textContent = cfg.path;
  document.getElementById("config-editor").value = cfg.content;
  document.getElementById("config-editor").disabled = !cfg.editable;
  configVersion = cfg.version;
  setConfigStatus("");
}

function setConfigStatus(msg, isError) {
  const s = document.getElementById("config-status");
  s.textContent = msg;
  s.style.color = isError ? "var(--err)" : "var(--muted)";
}

document.getElementById("config-validate").addEventListener("click", async () => {
  const content = document.getElementById("config-editor").value;
  try {
    await apiJSON("/dashboard/api/config/validate", { method: "POST", body: JSON.stringify({ content }) });
    setConfigStatus("valid");
  } catch (e) {
    setConfigStatus(e.message, true);
  }
});

document.getElementById("config-save").addEventListener("click", async () => {
  const content = document.getElementById("config-editor").value;
  setConfigStatus("saving…");
  try {
    const res = await apiJSON("/dashboard/api/config", {
      method: "PUT",
      body: JSON.stringify({ content, version: configVersion }),
    });
    configVersion = res.version;
    let msg = "applied";
    if (res.restart_required && res.restart_required.length) {
      msg += " (restart required for: " + res.restart_required.join(", ") + ")";
    }
    setConfigStatus(msg);
  } catch (e) {
    if (e.status === 409 && e.data && e.data.current_content) {
      setConfigStatus("conflict: config changed since you loaded it — reload before saving", true);
    } else {
      setConfigStatus(e.message, true);
    }
  }
});

document.getElementById("config-editor").addEventListener("keydown", (ev) => {
  if (ev.key === "Tab") {
    ev.preventDefault();
    const el = ev.target;
    const start = el.selectionStart;
    const end = el.selectionEnd;
    el.value = el.value.slice(0, start) + "  " + el.value.slice(end);
    el.selectionStart = el.selectionEnd = start + 2;
  }
  if ((ev.metaKey || ev.ctrlKey) && ev.key === "s") {
    ev.preventDefault();
    document.getElementById("config-save").click();
  }
});

// --- polling ------------------------------------------------------------------
setInterval(() => {
  if (!document.getElementById("auto-refresh").checked) return;
  refreshActiveTab();
}, 3000);

if (!getToken()) {
  showTokenPrompt();
} else {
  refreshActiveTab();
}
