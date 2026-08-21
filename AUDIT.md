# Audit — homelab/heka

_generated overnight_

## Verdict: lean  (~80 LOC removable)

heka is a rare thing: a codebase that actually obeys its own YAGNI spec. Almost
all the complexity here is *essential* — mandated by SPEC.md (hot-reload with
live-state reuse, cross-dialect token normalization, affinity/HRW). Very little
to cut. Findings below are minor.

## What to cut  (ranked, highest payoff first)

- **Duplicated key-state snapshot/reset logic** — `server.go:291-345` (handleStatus/handleReset over `State.Pools`/`SidecarStates`) vs `app.go:290-352` (StatusSnapshot/ResetStatus/ResetStatusKey) + `dashboard.go:199-225` → one path. `State.Pools`/`SidecarStates` exist only so the gateway `/status` can avoid going through `app`; both sides re-implement snapshot + reset-by-key. Have `server` delegate to an app-provided snapshot/reset and drop the two `State` maps.
- **`Backend` interface, single impl** — `dashboard.go:34-44` (11 methods, only `*app.App` implements it, "tests can supply a fake") → use `*app.App` directly and let tests hit the real thing (or keep a tiny local iface in the test file). Idiomatic-Go, so this is defensible; low payoff.
- **Repeated pointer-override config scaffolding** — `config.go` History/Dashboard/Capture/Rotation each carry a `*Struct` + `Params` + `apply`/`Default*` quartet (4x the same shape) → genuine DRY smell, but each section's defaults differ and generics would obscure it. Leave unless a 5th section lands.

## Correctness flags

- [low] `addAttr` reuses `groups` backing array via `append(groups, a.Key)` when recursing into a grouped slog attr — sibling group attrs can clobber each other's key prefix. Dormant: heka never calls `WithGroup`/`slog.Group`, so it only bites if reused. — `internal/history/log.go:76`

## Notes

Overall shape is clean and idiomatic Go: atomic pointer swap for reload, nil-safe
`*Store`/`*sink`/`*usageScanner` so callers skip nil checks, constant-time token
compare, bcrypt-as-rate-limit, session cookie scoped to `/dashboard`. The genuinely
gnarly parts (usage.go dialect normalization, affinity.go canonical hashing,
keypool HRW, app.go fingerprint-based reuse) are all load-bearing per spec, not
speculative — don't "simplify" them without re-reading §4.3.1/§4.7.1/§4.9. The
`scripts/gen-pi-models.mjs` helper is standalone client tooling, out of the binary,
and documented — fine as-is. No dead flexibility, no unused deps (yaml + bcrypt only),
no reinvented stdlib. Leave it alone.
