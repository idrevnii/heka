"use strict";

// --- sign in ---------------------------------------------------------------
// The session lives in an HttpOnly cookie the server sets on login, so this
// file never touches a credential after the form is submitted: every fetch
// below just rides the cookie, and a 401 means the session is gone and the
// form goes back up.
let signedIn = false;
let authEpoch = 0;

async function api(path, opts) {
  const epoch = authEpoch;
  opts = opts || {};
  const headers = Object.assign({}, opts.headers || {});
  if (opts.body && !headers["Content-Type"]) headers["Content-Type"] = "application/json";
  const res = await fetch(path, Object.assign({}, opts, { headers }));
  if (epoch !== authEpoch) throw Object.assign(new Error("Session changed."), { status: 401 });
  if (res.status === 401) {
    showLogin();
    const err = new Error("Session expired. Sign in again.");
    err.status = 401;
    throw err;
  }
  if (res.ok && !signedIn) hideLogin();
  return res;
}

async function apiJSON(path, opts) {
  const epoch = authEpoch;
  const res = await api(path, opts);
  const data = await res.json().catch(() => null);
  if (epoch !== authEpoch) throw Object.assign(new Error("Session changed."), { status: 401 });
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
  authEpoch++;
  signedIn = false;
  const login = document.getElementById("login");
  document.getElementById("disable-dialog").close();
  if (!login.open) login.showModal();
  document.getElementById("logout").classList.add("hidden");
  document.getElementById("login-password").value = "";
}

function hideLogin() {
  signedIn = true;
  document.getElementById("login").close();
  document.getElementById("logout").classList.remove("hidden");
  document.getElementById("login-error").textContent = "";
  document.getElementById("login-password").value = "";
}

document.getElementById("login").addEventListener("cancel", (ev) => ev.preventDefault());

document.getElementById("login-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const submit = ev.currentTarget.querySelector('button[type="submit"]');
  if (submit.disabled) return;
  submit.disabled = true;
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
  } finally {
    submit.disabled = false;
    document.getElementById("login-password").value = "";
  }
  if (!res.ok) {
    const data = await res.json().catch(() => null);
    err.textContent = (data && data.error && data.error.message) || res.statusText;
    document.getElementById("login-password").value = "";
    return;
  }
  authEpoch++;
  hideLogin();
  refreshActiveTab();
});

document.getElementById("logout").addEventListener("click", async () => {
  try {
    const res = await fetch("/dashboard/api/logout", { method: "POST" });
    if (!res.ok) throw new Error(res.statusText);
    showLogin();
  } catch (err) {
    showNotice("Could not log out: " + err.message, true);
  }
});

// --- tabs ------------------------------------------------------------------
const tabs = ["overview", "requests", "config"];
const pageCopy = {
  overview: ["Overview", "Your providers, keys and traffic at a glance."],
  requests: ["Requests", "Follow the traffic. Inspect the details."],
  config: ["Configuration", "Edit, validate and apply your gateway settings."],
};
function activateTab(name) {
  for (const t of tabs) {
    document.getElementById("tab-" + t).classList.toggle("active", t === name);
  }
  document.querySelectorAll("#tabs button").forEach((b) => {
    b.classList.toggle("active", b.dataset.tab === name);
    if (b.dataset.tab === name) b.setAttribute("aria-current", "page");
    else b.removeAttribute("aria-current");
  });
  document.getElementById("page-title").textContent = pageCopy[name][0];
  document.getElementById("page-description").textContent = pageCopy[name][1];
  document.title = pageCopy[name][0] + " · heka";
  updateRefreshStatus();
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

function showNotice(message, isError = false) {
  const notice = document.getElementById("notice");
  notice.querySelector("span").textContent = message;
  notice.className = "notice" + (isError ? " error" : "");
}
document.getElementById("notice-dismiss").addEventListener("click", () => document.getElementById("notice").classList.add("hidden"));

let lastUpdated = null;
function updateRefreshStatus() {
  document.getElementById("refresh-status").textContent = activeTabName() === "config" ? "Manual apply" :
    !document.getElementById("auto-refresh").checked ? "Updates paused" :
    lastUpdated ? "Updated " + fmtTime(lastUpdated) : "Connecting…";
}

function emptyState(title, description) {
  return el("div", { class: "empty-state" }, [
    el("span", { class: "empty-mark", "aria-hidden": "true" }, ["✓"]),
    el("div", {}, [el("strong", {}, [title]), description]),
  ]);
}

// --- overview ----------------------------------------------------------------
let overviewLoading = false;
let keyActionPending = false;

async function loadOverview() {
  if (overviewLoading) return;
  overviewLoading = true;
  try {
    const [ov, status, errs] = await Promise.all([
      apiJSON("/dashboard/api/overview"),
      apiJSON("/dashboard/api/status"),
      apiJSON("/dashboard/api/errors?limit=20"),
    ]);
    const tiles = document.getElementById("stat-tiles");
    clear(tiles);
    const w = ov.window || {};
    const minutes = Math.floor((ov.uptime_s || 0) / 60);
    const items = [
      ["Total requests", fmtNum(ov.total), "Since gateway started"],
      ["Error rate", w.count ? Math.round(w.error_rate * 1000) / 10 + "%" : "–", "Last 5 minutes"],
      ["p95 latency", w.count && w.p95_ms != null ? w.p95_ms + " ms" : "–", "Last 5 minutes"],
      ["Uptime", minutes < 60 ? minutes + "m" : Math.floor(minutes / 60) + "h " + minutes % 60 + "m", "Current session"],
      ["History used", fmtNum(ov.history_used) + " / " + fmtNum(ov.history_size), "Requests retained"],
    ];
    for (const [label, value, hint] of items) {
      tiles.appendChild(el("div", { class: "tile" }, [
        el("div", { class: "label" }, [label]),
        el("div", { class: "value" }, [String(value)]),
        el("div", { class: "hint" }, [hint]),
      ]));
    }

    const proxies = document.getElementById("proxies");
    clear(proxies);
    const providers = Object.entries(status.providers || {});
    document.getElementById("provider-count").textContent = providers.length + " providers · " +
      providers.reduce((sum, [, keys]) => sum + keys.length, 0) + " keys";
    for (const [name, keys] of providers) {
      const active = keys.filter(k => k.state === "active").length;
      const block = el("section", { class: "proxy-block", "aria-label": name }, [
        el("div", { class: "provider-heading" }, [
          el("span", { class: "provider-mark", "aria-hidden": "true" }, [name.slice(0, 2).toUpperCase()]),
          el("div", {}, [el("h3", {}, [name]), el("div", { class: "provider-route" }, ["/" + name])]),
          el("span", { class: "provider-summary" }, [active + " of " + keys.length + " active"]),
        ]),
      ]);
      const table = el("table", { class: "keys-table", "aria-label": name + " keys" }, [
        el("thead", {}, [el("tr", {}, ["Key", "State", "Success", "Failed", "Cache hit", "Actions"].map(
          title => el("th", { scope: "col" }, [title])
        ))]),
      ]);
      const tbody = el("tbody", {}, []);
      const canCheck = !!(status.check && status.check[name]);
      const available = new Set(keys.filter(k => ["active", "cooldown"].includes(k.state)).map(k => k.id));
      for (const k of keys) {
        const remaining = available.size - (available.has(k.id) ? 1 : 0);
        tbody.appendChild(keyRow(name, k, canCheck, remaining));
        if (k.last_error) tbody.appendChild(keyErrorRow(name, k));
      }
      table.appendChild(tbody);
      block.appendChild(el("div", { class: "table-scroll", tabindex: "0", role: "region", "aria-label": name + " key table" }, [table]));
      proxies.appendChild(block);
    }
    if (status.sidecars && Object.keys(status.sidecars).length) {
      proxies.appendChild(el("section", { class: "proxy-block", "aria-label": "Sidecars" }, [
        el("div", { class: "provider-heading" }, [el("h3", {}, ["Sidecars"])]),
        el("div", { class: "sidecars" }, Object.entries(status.sidecars).map(([name, state]) =>
          el("span", { class: "pill " + state }, [name + " · " + state])
        )),
      ]));
    }
    if (!proxies.children.length) {
      proxies.appendChild(emptyState("No providers yet", "Add a provider in Configuration to start routing requests."));
    }

    const errBox = document.getElementById("errors");
    clear(errBox);
    if (!errs || !errs.length) {
      errBox.appendChild(emptyState("All clear", "No recent gateway errors. New events will appear here."));
    } else {
      errBox.appendChild(el("table", { "aria-label": "Recent errors" }, [
        el("thead", {}, [el("tr", {}, ["Time", "Level", "Message"].map(title => el("th", { scope: "col" }, [title])))]),
        el("tbody", {}, errs.map(e => el("tr", {}, [
          el("td", {}, [fmtTime(e.time)]),
          el("td", { class: e.level === "WARN" ? "status-4xx" : "status-err" }, [e.level]),
          el("td", {}, [e.msg, ...(e.attrs && Object.keys(e.attrs).length ? [el("pre", { class: "detail" }, [JSON.stringify(e.attrs, null, 2)])] : [])]),
        ]))),
      ]));
    }
    lastUpdated = new Date();
    updateRefreshStatus();
  } catch (err) {
    if (err.status !== 401) {
      showNotice("Could not refresh gateway status: " + err.message, true);
      document.getElementById("refresh-status").textContent = "Connection interrupted";
    }
  } finally {
    overviewLoading = false;
  }
}

// --- requests ------------------------------------------------------------------
let selectedRequestId = null;
let requestsLoadID = 0;

async function loadRequests() {
  const loadID = ++requestsLoadID;
  const onlyErrors = document.getElementById("only-errors").checked;
  let list;
  try {
    list = await apiJSON("/dashboard/api/requests?limit=100" + (onlyErrors ? "&errors=1" : ""));
  } catch (e) {
    if (loadID !== requestsLoadID) return;
    if (e.status !== 401) showNotice("Could not load requests: " + e.message, true);
    return;
  }
  if (loadID !== requestsLoadID) return;
  const tbody = document.querySelector("#requests-table tbody");
  clear(tbody);
  document.getElementById("request-count").textContent = (list || []).length + " recent requests";
  if (!list || !list.length) {
    tbody.appendChild(el("tr", {}, [el("td", { colspan: "8" }, [
      emptyState(onlyErrors ? "No failed requests" : "No requests yet", onlyErrors ? "No errors match this view." : "Requests will appear here as traffic reaches the gateway."),
    ])]));
  }
  for (const r of list || []) {
    const tr = el("tr", { "data-id": String(r.id), class: r.id === selectedRequestId ? "selected" : "" }, [
      el("td", {}, [fmtTime(r.time)]),
      el("td", {}, [r.route || ""]),
      el("td", {}, [r.method || ""]),
      el("td", { title: r.path || "" }, [el("button", { class: "request-select", "aria-label": "Inspect request " + r.id + ": " + r.path }, [r.path || "/"])]),
      el("td", { class: statusClass(r.status) }, [String(r.status)]),
      el("td", {}, [String(r.attempts)]),
      el("td", {}, [String(r.duration_ms)]),
      el("td", { class: "cache", title: tokensTitle(r) }, [fmtTokens(r)]),
    ]);
    tr.addEventListener("click", () => showRequestDetail(r.id));
    tbody.appendChild(tr);
  }
  lastUpdated = new Date();
  updateRefreshStatus();
}

// --- key rows -----------------------------------------------------------------
// Which key-error panels are open, as "provider#index". The overview
// re-renders from scratch every 3s, so open panels have to be remembered
// here or they'd snap shut under the poll.
const expandedKeys = new Set();

function keyTag(provider, k) {
  return provider + "#" + k.id;
}

function keyRow(provider, k, canCheck, remaining) {
  const actions = el("td", { class: "key-actions" }, []);
  if (k.last_error) {
    const toggle = el("button", { class: "link", title: "Show the provider's error", "aria-expanded": String(expandedKeys.has(keyTag(provider, k))) }, [
      expandedKeys.has(keyTag(provider, k)) ? "Hide details" : "Details",
    ]);
    toggle.addEventListener("click", () => {
      const tag = keyTag(provider, k);
      if (expandedKeys.has(tag)) expandedKeys.delete(tag);
      else expandedKeys.add(tag);
      loadOverview();
    });
    actions.appendChild(toggle);
  }
  if (k.state !== "blocked") {
    const reset = el("button", { class: "link", title: "Clear cooldown or automatic disable for this key" }, ["Reset"]);
    reset.addEventListener("click", () => resetKey(provider, k, reset));
    actions.appendChild(reset);
    if (canCheck) {
      const check = el("button", { class: "link", title: "Ask the provider for one token with this key" }, ["Check"]);
      check.addEventListener("click", () => checkKey(provider, k, check));
      actions.appendChild(check);
    }
    const disable = el("button", { class: "danger-link", title: "Permanently disable this key in heka", "aria-label": "Disable " + provider + " key " + (k.index + 1) + " forever" }, ["Disable"]);
    disable.addEventListener("click", () => openDisableDialog(provider, k, remaining));
    actions.appendChild(disable);
  } else {
    actions.appendChild(el("span", { class: "action-note" }, ["Cannot be reset"]));
  }

  const labels = { active: "Active", cooldown: "Cooldown", disabled: "Disabled", blocked: "Off forever" };
  const state = el("td", {}, [el("span", { class: "pill " + k.state, title: k.state === "blocked" ? "Permanently disabled in heka" : labels[k.state] }, [labels[k.state] || k.state])]);
  if (k.cooldown_until) {
    const seconds = Math.max(0, Math.ceil((new Date(k.cooldown_until) - Date.now()) / 1000));
    state.appendChild(el("span", { class: "cooldown-time", title: "Cooldown expires at " + fmtTime(k.cooldown_until) }, [" " + (seconds < 60 ? seconds + "s" : Math.ceil(seconds / 60) + "m")]));
  }
  return el("tr", { class: k.state === "blocked" ? "key-blocked" : "" }, [
    el("td", { title: k.key }, [el("span", { class: "key-number" }, [String(k.index + 1).padStart(2, "0") + " "]), el("span", { class: "key-secret" }, [k.key])]),
    state,
    el("td", {}, [fmtNum(k.successes)]),
    el("td", {}, [fmtNum(k.failures)]),
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
  if (keyActionPending) return;
  keyActionPending = true;
  button.disabled = true;
  try {
    await apiJSON(
      "/dashboard/api/status/reset?provider=" + encodeURIComponent(provider) + "&key=" + k.index,
      { method: "POST" }
    );
    expandedKeys.delete(keyTag(provider, k));
    showNotice(provider + " · " + k.key + ": runtime state reset.");
  } catch (err) {
    showNotice("Could not reset key: " + err.message, true);
    button.disabled = false;
    return;
  } finally {
    keyActionPending = false;
  }
  loadOverview();
}

// checkKey spends one real (one-token) upstream request on this key. The
// verdict is recorded in the pool server-side, so the answer shows up in the
// state pill, the counters and the why? panel — nothing to render here.
async function checkKey(provider, k, button) {
  if (keyActionPending) return;
  keyActionPending = true;
  button.disabled = true;
  button.textContent = "Checking…";
  try {
    const result = await apiJSON(
      "/dashboard/api/status/check?provider=" + encodeURIComponent(provider) + "&key=" + k.index,
      { method: "POST" }
    );
    if (!result.ok) expandedKeys.add(keyTag(provider, k));
    showNotice(provider + " · " + k.key + (result.ok ? ": check passed." : ": check failed" + (result.status ? " (HTTP " + result.status + ")." : ". " + result.message)), !result.ok);
  } catch (err) {
    showNotice("Could not check key: " + err.message, true);
    button.textContent = "Check";
    button.disabled = false;
    return;
  } finally {
    keyActionPending = false;
  }
  loadOverview();
}

let disableTarget = null;
const disableDialog = document.getElementById("disable-dialog");
function openDisableDialog(provider, k, remaining) {
  if (keyActionPending) return;
  disableTarget = { provider, key: k };
  document.getElementById("disable-target").textContent = provider + " · key " + (k.index + 1) + " · " + k.key;
  document.getElementById("disable-last-key").classList.toggle("hidden", remaining > 0);
  document.getElementById("disable-error").textContent = "";
  disableDialog.showModal();
  document.getElementById("disable-cancel").focus();
}
document.getElementById("disable-cancel").addEventListener("click", () => disableDialog.close());
disableDialog.addEventListener("cancel", ev => { if (keyActionPending) ev.preventDefault(); });
disableDialog.addEventListener("close", () => { disableTarget = null; });
document.getElementById("disable-form").addEventListener("submit", async ev => {
  ev.preventDefault();
  if (!disableTarget || keyActionPending) return;
  const { provider, key } = disableTarget;
  const confirm = document.getElementById("disable-confirm");
  const cancel = document.getElementById("disable-cancel");
  keyActionPending = true;
  confirm.disabled = cancel.disabled = true;
  confirm.textContent = "Disabling…";
  document.getElementById("disable-error").textContent = "";
  try {
    await apiJSON("/dashboard/api/status/disable?provider=" + encodeURIComponent(provider) + "&key=" + key.index, {
      method: "POST", body: JSON.stringify({ id: key.id }),
    });
    disableDialog.close();
    showNotice(provider + " · " + key.key + " is permanently disabled in heka.");
    await loadOverview();
  } catch (err) {
    document.getElementById("disable-error").textContent = err.message;
  } finally {
    keyActionPending = false;
    confirm.disabled = cancel.disabled = false;
    confirm.textContent = "Disable forever";
  }
});

// Cache hit rate is the payoff of key affinity: a pool answering from warm
// prompt caches reads most of its input tokens instead of paying for them.
function fmtCache(k) {
  if (k.cache_hit_rate === null || k.cache_hit_rate === undefined) return "–";
  return Math.round(k.cache_hit_rate * 100) + "%";
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
  document.querySelectorAll("#requests-table tr[data-id]").forEach(tr => tr.classList.toggle("selected", tr.dataset.id === String(id)));
  const pre = document.getElementById("request-detail");
  pre.textContent = "loading…";
  let rec;
  try {
    rec = await apiJSON("/dashboard/api/requests/" + id);
  } catch (e) {
    if (selectedRequestId !== id) return;
    pre.textContent = "failed to load: " + e.message;
    return;
  }
  if (selectedRequestId !== id) return;
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
  let text = b.text || "";
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
let configSaving = false;
let configEditable = false;
let configLoadID = 0;

function ensureEditor(initialText, readOnly) {
  if (editor) return editor;
  const container = document.getElementById("config-editor-container");
  editor = window.HekaEditor.create(
    container,
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
  container.querySelector(".cm-content").setAttribute("aria-label", "Gateway configuration (YAML)");
  container.querySelector(".cm-scroller").setAttribute("tabindex", "0");
  return editor;
}

// Explicit reload may discard an existing draft, but never text entered
// after that reload started.
async function loadConfig(force) {
  if (configSaving) return;
  if (editor && configDirty && !force) {
    return; // never clobber an in-progress edit
  }
  const loadID = ++configLoadID;
  const before = editor && editor.getValue();
  let cfg;
  try {
    cfg = await apiJSON("/dashboard/api/config");
  } catch (e) {
    if (loadID !== configLoadID) return;
    if (e.status !== 401) setConfigStatus("Could not load configuration: " + e.message, true);
    return;
  }
  if (loadID !== configLoadID || configSaving || (editor && editor.getValue() !== before)) return;
  if (editor && configDirty && !force) return;
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
  configEditable = cfg.editable;
  document.getElementById("config-reload").disabled = false;
  document.getElementById("config-validate").disabled = false;
  document.getElementById("config-save").disabled = !cfg.editable;
  setConfigStatus("");
}

function setConfigStatus(msg, isError) {
  const s = document.getElementById("config-status");
  s.textContent = msg;
  s.classList.toggle("error", !!isError);
}

// Parses "... line 12: ..." out of a config.Parse error (yaml.v3's error
// messages include a line number) and jumps the editor there.
function jumpToErrorLine(message) {
  const m = /line (\d+)/i.exec(message || "");
  if (m && editor) editor.gotoLine(parseInt(m[1], 10));
}

document.getElementById("config-validate").addEventListener("click", async () => {
  if (!editor) return;
  const content = editor.getValue();
  try {
    await apiJSON("/dashboard/api/config/validate", { method: "POST", body: JSON.stringify({ content }) });
    if (editor.getValue() !== content) return;
    setConfigStatus("valid");
  } catch (e) {
    if (editor.getValue() !== content) return;
    setConfigStatus(e.message, true);
    jumpToErrorLine(e.message);
  }
});

document.getElementById("config-save").addEventListener("click", async () => {
  if (!editor || configSaving) return;
  configSaving = true;
  configLoadID++;
  document.getElementById("config-save").disabled = true;
  document.getElementById("config-reload").disabled = true;
  const content = editor.getValue();
  setConfigStatus("saving…");
  try {
    const res = await apiJSON("/dashboard/api/config", {
      method: "PUT",
      body: JSON.stringify({ content, version: configVersion }),
    });
    configVersion = res.version;
    configBaseline = content;
    configDirty = editor.getValue() !== content;
    let msg = "applied";
    if (res.restart_required && res.restart_required.length) {
      msg += " (restart required for: " + res.restart_required.join(", ") + ")";
    }
    if (configDirty) msg += " · newer edits are still unsaved";
    setConfigStatus(msg);
  } catch (e) {
    if (e.status === 409) {
      setConfigStatus("conflict: config changed since you loaded it — reload before saving", true);
    } else {
      setConfigStatus(e.message, true);
      jumpToErrorLine(e.message);
    }
  } finally {
    configSaving = false;
    document.getElementById("config-save").disabled = !configEditable;
    document.getElementById("config-reload").disabled = false;
  }
});

document.getElementById("config-reload").addEventListener("click", () => {
  if (configDirty && !window.confirm("Discard unsaved changes and reload configuration from disk?")) return;
  loadConfig(true);
});

// --- polling ------------------------------------------------------------------
setInterval(() => {
  if (!signedIn) return;
  if (!document.getElementById("auto-refresh").checked) return;
  // The config tab is an editing session, not a live view — polling it
  // would either discard in-progress edits or (with the dirty-check above)
  // just be a no-op, so skip it outright.
  if (activeTabName() === "config") return;
  if (document.hidden || keyActionPending || disableDialog.open || document.activeElement.closest(".key-actions, .request-select")) return;
  refreshActiveTab();
}, 3000);

document.getElementById("auto-refresh").addEventListener("change", () => {
  updateRefreshStatus();
  if (document.getElementById("auto-refresh").checked) refreshActiveTab();
});

window.addEventListener("beforeunload", (ev) => {
  if (configDirty || configSaving) {
    ev.preventDefault();
    ev.returnValue = "";
  }
});

// There is no "am I signed in?" endpoint: the first data fetch answers it —
// it either renders (api() reveals the page) or 401s and api() puts the
// sign-in form up.
refreshActiveTab();
