"use strict";

// --- sign in ---------------------------------------------------------------
// The session lives in an HttpOnly cookie the server sets on login, so this
// file never touches a credential after the form is submitted: every fetch
// below just rides the cookie, and a 401 means the session is gone and the
// form goes back up.
let signedIn = false;

async function api(path, opts) {
  opts = opts || {};
  const headers = Object.assign({}, opts.headers || {});
  if (opts.body && !headers["Content-Type"]) headers["Content-Type"] = "application/json";
  const res = await fetch(path, Object.assign({}, opts, { headers }));
  if (res.status === 401) {
    showLogin();
    throw new Error("unauthorized");
  }
  if (!signedIn) hideLogin();
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

function showLogin() {
  signedIn = false;
  document.getElementById("login").classList.remove("hidden");
  document.getElementById("logout").classList.add("hidden");
  document.getElementById("login-password").value = "";
}

function hideLogin() {
  signedIn = true;
  document.getElementById("login").classList.add("hidden");
  document.getElementById("logout").classList.remove("hidden");
  document.getElementById("login-error").textContent = "";
}

document.getElementById("login-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const user = document.getElementById("login-user").value.trim();
  const password = document.getElementById("login-password").value;
  const err = document.getElementById("login-error");
  err.textContent = "";
  let res;
  try {
    // Not api(): a failed login is a plain 401 to show inline, not a
    // session expiry that should re-open the form.
    res = await fetch("/dashboard/api/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ user, password }),
    });
  } catch (e) {
    err.textContent = "network error";
    return;
  }
  if (!res.ok) {
    const data = await res.json().catch(() => null);
    err.textContent = (data && data.error && data.error.message) || res.statusText;
    document.getElementById("login-password").value = "";
    return;
  }
  hideLogin();
  refreshActiveTab();
});

document.getElementById("logout").addEventListener("click", async () => {
  await fetch("/dashboard/api/logout", { method: "POST" }).catch(() => {});
  showLogin();
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
        tbody.appendChild(keyRow(name, k));
        if (k.last_error) tbody.appendChild(keyErrorRow(name, k));
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
      el("td", { class: "cache", title: tokensTitle(r) }, [fmtTokens(r)]),
    ]);
    tr.addEventListener("click", () => showRequestDetail(r.id));
    tbody.appendChild(tr);
  }
}

// --- key rows -----------------------------------------------------------------
// Which key-error panels are open, as "provider#index". The overview
// re-renders from scratch every 3s, so open panels have to be remembered
// here or they'd snap shut under the poll.
const expandedKeys = new Set();

function keyTag(provider, k) {
  return provider + "#" + k.index;
}

function keyRow(provider, k) {
  const actions = el("td", { class: "key-actions" }, []);
  if (k.last_error) {
    const toggle = el("button", { class: "link", title: "show the provider's own error" }, [
      expandedKeys.has(keyTag(provider, k)) ? "hide error" : "why?",
    ]);
    toggle.addEventListener("click", () => {
      const tag = keyTag(provider, k);
      if (expandedKeys.has(tag)) expandedKeys.delete(tag);
      else expandedKeys.add(tag);
      loadOverview();
    });
    actions.appendChild(toggle);
  }
  const reset = el("button", { class: "link", title: "clear cooldown/disabled state for this key" }, ["reset"]);
  reset.addEventListener("click", () => resetKey(provider, k, reset));
  actions.appendChild(reset);

  return el("tr", {}, [
    el("td", {}, [k.key]),
    el("td", {}, [el("span", { class: "pill " + k.state }, [k.state])]),
    el("td", {}, [String(k.successes) + " ok"]),
    el("td", {}, [String(k.failures) + " fail"]),
    el("td", { class: "cache", title: cacheTitle(k) }, [fmtCache(k)]),
    actions,
  ]);
}

// keyErrorRow is the expandable panel under a key: the status code and the
// upstream's verbatim body, which is the only thing that says whether a
// disabled key is revoked, out of credit, or merely mistyped.
function keyErrorRow(provider, k) {
  const open = expandedKeys.has(keyTag(provider, k));
  const e = k.last_error;
  const head = e.reason + (e.status ? " · HTTP " + e.status : "") + " · " + fmtTime(e.at);
  return el("tr", { class: "key-error" + (open ? "" : " hidden") }, [
    el("td", { colspan: "6" }, [
      el("pre", { class: "detail" }, [head + "\n\n" + (prettyJSON(e.message) || "(no response body)")]),
    ]),
  ]);
}

function prettyJSON(text) {
  if (!text) return "";
  try {
    return JSON.stringify(JSON.parse(text), null, 2);
  } catch (err) {
    return text;
  }
}

async function resetKey(provider, k, button) {
  button.disabled = true;
  try {
    await apiJSON(
      "/dashboard/api/status/reset?provider=" + encodeURIComponent(provider) + "&key=" + k.index,
      { method: "POST" }
    );
    expandedKeys.delete(keyTag(provider, k));
  } catch (err) {
    button.textContent = "failed";
    button.title = err.message;
    button.disabled = false;
    return;
  }
  loadOverview();
}

// Cache hit rate is the payoff of key affinity: a pool answering from warm
// prompt caches reads most of its input tokens instead of paying for them.
function fmtCache(k) {
  if (k.cache_hit_rate === null || k.cache_hit_rate === undefined) return "–";
  return Math.round(k.cache_hit_rate * 100) + "% cached";
}

function cacheTitle(k) {
  return "input " + fmtNum(k.input_tokens) +
    " · cache read " + fmtNum(k.cache_read_tokens) +
    " · cache write " + fmtNum(k.cache_write_tokens) +
    " · output " + fmtNum(k.output_tokens);
}

// Kept short: this column shares a row with the path, and the full
// breakdown is one hover away.
function fmtTokens(r) {
  if (!r.input_tokens) return "";
  const pct = Math.round(((r.cache_read_tokens || 0) / r.input_tokens) * 100);
  return fmtNum(r.input_tokens) + " · " + pct + "%";
}

function tokensTitle(r) {
  if (!r.input_tokens) return "";
  return "input " + r.input_tokens + " (cache read " + (r.cache_read_tokens || 0) +
    ", cache write " + (r.cache_write_tokens || 0) + ") · output " + (r.output_tokens || 0);
}

function fmtNum(n) {
  n = n || 0;
  if (n < 1000) return String(n);
  if (n < 1000000) return (n / 1000).toFixed(1).replace(/\.0$/, "") + "k";
  return (n / 1000000).toFixed(1).replace(/\.0$/, "") + "M";
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
  if (rec.input_tokens) {
    lines.push("tokens: input=" + rec.input_tokens +
      " (cache read=" + (rec.cache_read_tokens || 0) +
      ", cache write=" + (rec.cache_write_tokens || 0) + ")" +
      " output=" + (rec.output_tokens || 0));
  }
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
// Editing uses the vendored CodeMirror bundle (codemirror.bundle.js,
// window.HekaEditor) for YAML syntax highlighting, line numbers and
// error-line jumping — see internal/dashboard/editor-src/README.md for how
// it's built. It's a local, self-contained asset: no CDN, no network access
// needed to load the dashboard.
let configVersion = "";
let editor = null;
// baseline is the last content we know the server has (freshly loaded or
// just saved); dirty means the editor has diverged from it. Both the
// polling refresh and tab-switch reloads must never overwrite an in-progress
// edit — that was the bug: the 3s poll called loadConfig() unconditionally
// even while the config tab was open, discarding whatever was being typed.
let configBaseline = "";
let configDirty = false;

function ensureEditor(initialText, readOnly) {
  if (editor) return editor;
  editor = window.HekaEditor.create(
    document.getElementById("config-editor-container"),
    initialText,
    {
      readOnly: readOnly,
      onChange: (text) => {
        const wasDirty = configDirty;
        configDirty = text !== configBaseline;
        if (configDirty && !wasDirty) setConfigStatus("unsaved changes");
        if (!configDirty) setConfigStatus("");
      },
      onSave: () => document.getElementById("config-save").click(),
    }
  );
  return editor;
}

// force=true is used right after a successful save, where we want to
// re-baseline even though the editor technically still holds "dirty"
// (just-saved) content.
async function loadConfig(force) {
  if (editor && configDirty && !force) {
    return; // never clobber an in-progress edit
  }
  let cfg;
  try {
    cfg = await apiJSON("/dashboard/api/config");
  } catch (e) {
    return;
  }
  document.getElementById("config-path").textContent = cfg.path;
  if (!editor) {
    ensureEditor(cfg.content, !cfg.editable);
  } else {
    editor.setValue(cfg.content);
    editor.setReadOnly(!cfg.editable);
  }
  configVersion = cfg.version;
  configBaseline = cfg.content;
  configDirty = false;
  setConfigStatus("");
}

function setConfigStatus(msg, isError) {
  const s = document.getElementById("config-status");
  s.textContent = msg;
  s.style.color = isError ? "var(--err)" : "var(--muted)";
}

// Parses "... line 12: ..." out of a config.Parse error (yaml.v3's error
// messages include a line number) and jumps the editor there.
function jumpToErrorLine(message) {
  const m = /line (\d+)/i.exec(message || "");
  if (m && editor) editor.gotoLine(parseInt(m[1], 10));
}

document.getElementById("config-validate").addEventListener("click", async () => {
  const content = editor.getValue();
  try {
    await apiJSON("/dashboard/api/config/validate", { method: "POST", body: JSON.stringify({ content }) });
    setConfigStatus("valid");
  } catch (e) {
    setConfigStatus(e.message, true);
    jumpToErrorLine(e.message);
  }
});

document.getElementById("config-save").addEventListener("click", async () => {
  const content = editor.getValue();
  setConfigStatus("saving…");
  try {
    const res = await apiJSON("/dashboard/api/config", {
      method: "PUT",
      body: JSON.stringify({ content, version: configVersion }),
    });
    configVersion = res.version;
    configBaseline = content;
    configDirty = false;
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
      jumpToErrorLine(e.message);
    }
  }
});

// --- polling ------------------------------------------------------------------
setInterval(() => {
  if (!signedIn) return;
  if (!document.getElementById("auto-refresh").checked) return;
  // The config tab is an editing session, not a live view — polling it
  // would either discard in-progress edits or (with the dirty-check above)
  // just be a no-op, so skip it outright.
  if (activeTabName() === "config") return;
  refreshActiveTab();
}, 3000);

window.addEventListener("beforeunload", (ev) => {
  if (configDirty) {
    ev.preventDefault();
    ev.returnValue = "";
  }
});

// There is no "am I signed in?" endpoint: the first data fetch answers it —
// it either renders (api() reveals the page) or 401s and api() puts the
// sign-in form up.
refreshActiveTab();
