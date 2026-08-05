# Pointing pi at Heka

[pi](https://github.com/earendil-works/pi-mono) ships the `opencode-go` model
catalog as a static JSON file inside the installed `@earendil-works/pi-ai`
package. There is no `/models` discovery call at runtime, so a gateway in front
of `https://opencode.ai/zen/go` is not enough: pi also needs a provider entry
that lists the models and points at Heka.

`scripts/gen-pi-models.mjs` generates that entry from pi's own catalog.

## Why two providers

pi picks the auth header from the model's API type, not from the provider:

| API type | Auth header | Catalog `baseUrl` |
|---|---|---|
| `openai-completions`, `openai-responses` | `Authorization: Bearer <key>` | `https://opencode.ai/zen/go/v1` |
| `anthropic-messages` | `x-api-key: <key>` | `https://opencode.ai/zen/go` |

`deploy/heka.yaml` therefore exposes the same key pool twice — `opencode-go`
(Bearer) and `opencode-go-anthropic` (`x-api-key`) — and the script splits the
catalog to match. Note that each Heka provider keeps its own key pool, so a
cooldown earned on one route is not visible to the other.

Overriding only `baseUrl` on pi's built-in `opencode-go` provider does not
work for the full catalog: pi rewrites every model in a provider to the same
URL, and the two API families need different path suffixes.

## Usage

```bash
node scripts/gen-pi-models.mjs --dry-run                        # preview
node scripts/gen-pi-models.mjs --base-url http://heka.local:8787
```

Writes `~/.pi/agent/models.json`, backing up any existing file to
`models.json.bak`. Other providers already in that file are preserved; only the
two generated ids are replaced.

| Flag | Default | Meaning |
|---|---|---|
| `--base-url` | `http://localhost:8787` | Heka base URL |
| `--token` | `$HEKA_TOKEN` | value for pi's `apiKey`; `$VAR` is resolved by pi at request time |
| `--route-openai` | `opencode-go` | Heka provider name for the Bearer route |
| `--route-anthropic` | `opencode-go-anthropic` | Heka provider name for the `x-api-key` route |
| `--provider-openai` | `heka-go` | provider id written into `models.json` |
| `--provider-anthropic` | `heka-go-anthropic` | provider id written into `models.json` |
| `--catalog` | auto-resolved | path to `opencode-go.json` |
| `--out` | `~/.pi/agent/models.json` | target file |
| `--dry-run` | off | print the merged file instead of writing it |

The `apiKey` is the **Heka gateway token**, not an OpenCode key — Heka accepts
it in `Authorization: Bearer`, `X-Api-Key`, or `X-Goog-Api-Key`, and injects a
real key from the pool upstream.

After generating, select the provider — either `pi --provider heka-go` or
`defaultProvider` in `~/.pi/agent/settings.json`. pi re-reads `models.json`
every time `/model` opens, so no restart is needed.

## Maintenance

The catalog lives inside the npm package and is replaced on every pi upgrade,
new models included. Re-run the script after upgrading pi.

## Limitations

pi adds `x-opencode-session` / `x-opencode-client` only for provider ids
`opencode` / `opencode-go` or hosts on `opencode.ai`. The script restores the
static `x-opencode-client: pi` header; the per-session one cannot be expressed
in `models.json` and needs a pi extension (`pi.registerProvider`) if upstream
requires it.
