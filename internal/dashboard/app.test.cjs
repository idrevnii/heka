const assert = require("node:assert/strict");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
const { test } = require("node:test");
const vm = require("node:vm");

const source = readFileSync(join(__dirname, "static/app.js"), "utf8");
const deferred = () => {
  let resolve;
  const promise = new Promise(r => { resolve = r; });
  return { promise, resolve };
};

function page() {
  const elements = new Map();
  const handlers = new Map();
  const element = id => {
    if (!elements.has(id)) elements.set(id, {
      value: "", open: false, dataset: { tab: "config" },
      classList: { add() {}, remove() {}, toggle() {} },
      addEventListener(event, fn) { handlers.set(id + ":" + event, fn); },
      close() { this.open = false; },
      showModal() { this.open = true; },
      querySelector() { return element(id + "-child"); },
    });
    return elements.get(id);
  };
  const editor = { text: "old", getValue() { return this.text; }, setValue(text) { this.text = text; }, setReadOnly() {} };
  const context = vm.createContext({
    document: { getElementById: element, querySelector: element, querySelectorAll: () => [] },
    window: { addEventListener(event, fn) { handlers.set("window:" + event, fn); } },
    fetch: () => new Promise(() => {}), // initial session probe remains pending
    setInterval() {}, testEditor: editor,
  });
  vm.runInContext(source, context);
  return { context, editor, handlers, read: code => vm.runInContext(code, context) };
}

test("typing during save remains dirty and simultaneous saves are suppressed", async () => {
  const p = page();
  p.read("editor = testEditor; configBaseline = 'old'; configVersion = 'v1';");
  p.editor.text = "save this";
  const response = deferred();
  let calls = 0;
  p.context.apiJSON = () => { calls++; return response.promise; };
  const first = p.handlers.get("config-save:click")();
  const second = p.handlers.get("config-save:click")();
  let warned = false;
  p.handlers.get("window:beforeunload")({ preventDefault() { warned = true; } });
  assert.equal(warned, true, "a pending save must warn before closing the page");
  p.editor.text = "typed while saving";
  p.read("configDirty = true");
  response.resolve({ version: "v2" });
  await Promise.all([first, second]);
  assert.equal(calls, 1);
  assert.equal(p.read("configDirty"), true);
  assert.equal(p.read("configBaseline"), "save this");
  assert.equal(p.read("configVersion"), "v2");
  assert.equal(p.editor.text, "typed while saving");
});

test("explicit reload preserves edits made while its request is pending", async () => {
  const p = page();
  p.read("editor = testEditor; configBaseline = 'old';");
  const response = deferred();
  p.context.apiJSON = () => response.promise;
  const loading = p.context.loadConfig(true);
  p.editor.text = "new draft";
  p.read("configDirty = true");
  response.resolve({ content: "server copy", version: "v2", editable: true, path: "/test" });
  await loading;
  assert.equal(p.editor.text, "new draft");
  assert.equal(p.read("configDirty"), true);
});

test("a validation response cannot mark a newer draft valid", async () => {
  const p = page();
  p.read("editor = testEditor");
  const response = deferred();
  p.context.apiJSON = () => response.promise;
  const validating = p.handlers.get("config-validate:click")();
  p.editor.text = "different unvalidated draft";
  response.resolve({ ok: true });
  await validating;
  assert.notEqual(p.context.document.getElementById("config-status").textContent, "valid");
});

test("late API response cannot sign the UI back in after session expiry", async () => {
  const p = page();
  p.read("signedIn = true");
  const response = deferred();
  p.context.fetch = () => response.promise;
  const request = p.context.api("/dashboard/api/overview");
  p.context.showLogin();
  response.resolve({ status: 200, ok: true });
  await assert.rejects(request, error => error.status === 401);
  assert.equal(p.read("signedIn"), false);
});

test("server errors do not establish a UI session", async () => {
  const p = page();
  p.context.fetch = async () => ({ status: 503, ok: false });
  await p.context.api("/dashboard/api/overview");
  assert.equal(p.read("signedIn"), false);
});

test("failed logout is reported without pretending the session ended", async () => {
  const p = page();
  p.read("signedIn = true");
  p.context.fetch = async () => { throw new Error("offline"); };
  await p.handlers.get("logout:click")();
  assert.equal(p.read("signedIn"), true);
});
