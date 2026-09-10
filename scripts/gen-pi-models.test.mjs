import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

test("invalid generator input never overwrites the destination", t => {
  const dir = mkdtempSync(join(tmpdir(), "heka-models-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const catalog = join(dir, "catalog.json");
  const out = join(dir, "models.json");
  writeFileSync(catalog, JSON.stringify({ "openai-completions": { m: { id: "m", name: "Mock" } } }));
  const run = (...args) => spawnSync(process.execPath, [fileURLToPath(new URL("./gen-pi-models.mjs", import.meta.url)), "--catalog", catalog, "--out", out, ...args], { encoding: "utf8" });
  writeFileSync(out, '{"providers":{"custom":{}}}');
  const original = readFileSync(out, "utf8");
  assert.equal(run("--base-url", "--dry-run").status, 2);
  assert.equal(readFileSync(out, "utf8"), original);
  const preview = run("--dry-run");
  assert.equal(preview.status, 0, preview.stderr);
  assert.ok(JSON.parse(preview.stdout).providers.custom);
  assert.ok(JSON.parse(preview.stdout).providers["heka-go"]);
  assert.equal(readFileSync(out, "utf8"), original);
  for (const invalid of ["null", "[]", '{"providers":[]}']) {
    writeFileSync(out, invalid);
    assert.notEqual(run().status, 0);
    assert.equal(readFileSync(out, "utf8"), invalid);
  }
});
