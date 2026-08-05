#!/usr/bin/env node
// Generates pi (~/.pi/agent/models.json) provider entries that point at a
// heka gateway instead of https://opencode.ai/zen/go directly.
//
// pi ships the opencode-go catalog as a static JSON inside the pi-ai package
// (no /models endpoint is ever called), and sends the API key in a header that
// depends on the model's api type: Authorization: Bearer for openai-*,
// x-api-key for anthropic-messages. heka exposes those as two routes with
// different key_in settings, so the catalog is split into two pi providers
// here. Re-run after upgrading pi to pick up new models.
//
//   node scripts/gen-pi-models.mjs --base-url http://heka.local:8787
//   node scripts/gen-pi-models.mjs --dry-run

import { createRequire } from "node:module";
import { homedir } from "node:os";
import { dirname, join } from "node:path";
import { copyFileSync, existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";

const DEFAULTS = {
  baseUrl: "http://localhost:8787",
  token: "$HEKA_TOKEN",
  // heka provider names (deploy/heka.yaml) -> route prefixes
  routeOpenai: "opencode-go",
  routeAnthropic: "opencode-go-anthropic",
  // pi provider ids written into models.json
  providerOpenai: "heka-go",
  providerAnthropic: "heka-go-anthropic",
  out: join(homedir(), ".pi", "agent", "models.json"),
  catalog: "",
};

function parseArgs(argv) {
  const opts = { ...DEFAULTS, dryRun: false };
  const flags = {
    "--base-url": "baseUrl",
    "--token": "token",
    "--route-openai": "routeOpenai",
    "--route-anthropic": "routeAnthropic",
    "--provider-openai": "providerOpenai",
    "--provider-anthropic": "providerAnthropic",
    "--out": "out",
    "--catalog": "catalog",
  };
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (arg === "--dry-run") {
      opts.dryRun = true;
      continue;
    }
    if (arg === "-h" || arg === "--help") {
      console.log(usage());
      process.exit(0);
    }
    const key = flags[arg];
    if (!key) {
      console.error(`unknown flag: ${arg}\n\n${usage()}`);
      process.exit(2);
    }
    const value = argv[++i];
    if (value === undefined) {
      console.error(`${arg} needs a value`);
      process.exit(2);
    }
    opts[key] = value;
  }
  opts.baseUrl = opts.baseUrl.replace(/\/+$/, "");
  return opts;
}

function usage() {
  return `usage: gen-pi-models.mjs [options]

  --base-url URL            heka base URL (default ${DEFAULTS.baseUrl})
  --token VALUE             gateway token for pi's apiKey field; "$VAR" reads
                            the env var at request time (default ${DEFAULTS.token})
  --route-openai NAME       heka provider route for Bearer auth (default ${DEFAULTS.routeOpenai})
  --route-anthropic NAME    heka provider route for x-api-key auth (default ${DEFAULTS.routeAnthropic})
  --provider-openai ID      pi provider id (default ${DEFAULTS.providerOpenai})
  --provider-anthropic ID   pi provider id (default ${DEFAULTS.providerAnthropic})
  --catalog PATH            override path to opencode-go.json
  --out PATH                target models.json (default ${DEFAULTS.out})
  --dry-run                 print the merged file instead of writing it`;
}

// The catalog lives inside the installed pi-ai package. Resolve it through
// node's module resolution first, then fall back to the well-known bun global
// install path.
function findCatalog(explicit) {
  const candidates = [];
  if (explicit) candidates.push(explicit);
  const require = createRequire(import.meta.url);
  for (const from of [process.cwd(), homedir()]) {
    try {
      const entry = require.resolve("@earendil-works/pi-ai", { paths: [from] });
      candidates.push(join(dirname(entry), "providers", "data", "opencode-go.json"));
    } catch {
      // not installed here; try the next root
    }
  }
  candidates.push(
    join(homedir(), ".bun/install/global/node_modules/@earendil-works/pi-ai/dist/providers/data/opencode-go.json"),
  );
  const found = candidates.find((path) => path && existsSync(path));
  if (!found) {
    console.error(
      `could not find opencode-go.json (looked in:\n  ${candidates.join("\n  ")})\npass --catalog PATH`,
    );
    process.exit(1);
  }
  return found;
}

// Drop the fields pi derives from the provider entry (provider, baseUrl) and
// keep `api` only where it differs from the provider default.
function toModel(entry, defaultApi) {
  const model = {};
  for (const key of [
    "id",
    "name",
    "api",
    "reasoning",
    "input",
    "contextWindow",
    "maxTokens",
    "cost",
    "compat",
    "thinkingLevelMap",
  ]) {
    if (entry[key] !== undefined) model[key] = entry[key];
  }
  if (model.api === defaultApi) delete model.api;
  return model;
}

const opts = parseArgs(process.argv.slice(2));
const catalogPath = findCatalog(opts.catalog);
const catalog = JSON.parse(readFileSync(catalogPath, "utf8"));

// openai-completions and openai-responses share both the /v1 base path and
// Bearer auth, so they go through the same heka route.
const groups = [
  {
    providerId: opts.providerOpenai,
    route: opts.routeOpenai,
    suffix: "/v1",
    defaultApi: "openai-completions",
    apis: ["openai-completions", "openai-responses"],
  },
  {
    providerId: opts.providerAnthropic,
    route: opts.routeAnthropic,
    suffix: "",
    defaultApi: "anthropic-messages",
    apis: ["anthropic-messages"],
  },
];

const providers = {};
const summary = [];
for (const group of groups) {
  const models = group.apis
    .flatMap((api) => Object.values(catalog[api] ?? {}))
    .map((entry) => toModel(entry, group.defaultApi))
    .sort((a, b) => a.id.localeCompare(b.id));
  if (models.length === 0) continue;
  providers[group.providerId] = {
    name: `${group.providerId} (heka)`,
    baseUrl: `${opts.baseUrl}/${group.route}${group.suffix}`,
    api: group.defaultApi,
    apiKey: opts.token,
    // pi only attaches x-opencode-* to provider ids opencode/opencode-go or
    // hosts on opencode.ai; the client tag is static so we can restore it, the
    // per-session one is not available from models.json.
    headers: { "x-opencode-client": "pi" },
    models,
  };
  summary.push(`${group.providerId}: ${models.length} models -> ${providers[group.providerId].baseUrl}`);
}

let existing = {};
if (existsSync(opts.out)) {
  try {
    existing = JSON.parse(readFileSync(opts.out, "utf8"));
  } catch (err) {
    console.error(`${opts.out} is not valid JSON: ${err.message}`);
    process.exit(1);
  }
}

// Merge so hand-written providers in models.json survive; only the two
// generated ids are replaced.
const merged = {
  ...existing,
  providers: { ...(existing.providers ?? {}), ...providers },
};
const json = `${JSON.stringify(merged, null, 2)}\n`;

if (opts.dryRun) {
  process.stdout.write(json);
  console.error(`\n# catalog: ${catalogPath}\n# ${summary.join("\n# ")}\n# (dry run, ${opts.out} not written)`);
  process.exit(0);
}

mkdirSync(dirname(opts.out), { recursive: true });
if (existsSync(opts.out)) {
  copyFileSync(opts.out, `${opts.out}.bak`);
  console.error(`backed up ${opts.out} -> ${opts.out}.bak`);
}
writeFileSync(opts.out, json);
console.error(`catalog: ${catalogPath}`);
console.error(summary.join("\n"));
console.error(`wrote ${opts.out}`);
