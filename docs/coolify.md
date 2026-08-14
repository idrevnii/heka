# Deploying Heka with CLIProxyAPI on Coolify

This deployment runs Heka and CLIProxyAPI as separate containers in one
Docker Compose project. Heka is the only client-facing service. CLIProxyAPI is
reachable only from the private Compose network.

## Coolify resource

Create a Docker Compose application from the private GitHub repository:

- repository: `idrevnii/heka`
- branch: `main`
- base directory: `/`
- Compose file: `/docker-compose.yml`
- public domain: none

The default host binding is `10.40.0.10:8787`. Coolify generates stable,
64-character values for both `SERVICE_PASSWORD_64_HEKA` and
`SERVICE_PASSWORD_64_CLIPROXY`.

The Heka token is the value of `SERVICE_PASSWORD_64_HEKA` shown in Coolify.
Clients use it as their API key. The CLIProxyAPI key is internal and should
not be given to clients.

The dashboard has its own login, separate from that token: set `HEKA_USER`
(defaults to `admin`) and `HEKA_PASSWORD_HASH` in Coolify's environment. The
hash is bcrypt — generate it once with

```sh
printf '%s' 'your-password' | docker compose run --rm --no-deps -T heka -hash
```

and paste the `$2a$...` line into `HEKA_PASSWORD_HASH`. Leave both unset and
`/dashboard` stays off.

`deploy/heka.yaml` in the repo is only a **first-boot seed**. The live,
authoritative config is `/data/apps/heka/config/heka.yaml` on the host,
bind-mounted into the `heka` container at `/etc/heka/heka.yaml` (a one-shot
`heka-init` service in the compose file `chown`s that directory to heka's
non-root uid before the gateway starts). On a fresh volume it's seeded from
`deploy/heka.yaml`; after that, editing the file in the repo has no further
effect on an already-deployed instance.

Ongoing changes — adding a provider, rotating keys, tweaking rotation
policy — go through either of:

- the dashboard's config editor at `http://10.40.0.10:8787/dashboard` — the
  page itself is unauthenticated (it carries no secrets, it is just the
  sign-in form), and signing in with `HEKA_USER` / the dashboard password
  sets an `HttpOnly` session cookie that authorizes the API calls behind it;
- editing `/data/apps/heka/config/heka.yaml` directly on the host and
  waiting for Heka's file watcher to pick it up (within `dashboard.watch`,
  10s by default), or sending it `SIGHUP`.

Both paths hot-reload: provider key pools and sidecar processes whose
config didn't change keep running untouched (cooldown/disabled state and
success/failure counters survive), and in-flight requests are unaffected.
Only a change to `listen` needs an actual restart — the dashboard's save
response flags this via `restart_required`. **The live config file may
contain plaintext provider API keys** — the `${VAR}` indirection used in
`deploy/heka.yaml` is no longer required once the file is live and edited
via the dashboard, though it still works. Treat that file with the same
sensitivity as the Coolify secrets it replaces.

CLIProxyAPI's own (non-secret) config still lives in `docker-compose.yml`
under `configs.cliproxy-config`; commit and redeploy for changes there.

## OAuth login

OAuth credentials are stored in
`/data/apps/heka/cliproxy-auth` on the persistent `apps-data` Incus volume.
Run login commands on the server with the same pinned CLIProxyAPI image and
bind mount.

### Grok

Open an SSH tunnel from the workstation and keep it running:

```sh
ssh -L 56121:127.0.0.1:56121 apps
```

In another terminal, start the login flow on the server:

```sh
ssh -t apps \
  "sudo docker run --rm -it --network host \
  -v /data/apps/heka/cliproxy-auth:/root/.cli-proxy-api \
  eceasy/cli-proxy-api:v7.2.100 \
  ./CLIProxyAPI --xai-login --no-browser"
```

Open the printed URL locally and finish authorization. The callback travels
through the SSH tunnel to port `56121` on the server.

### Codex

Use callback port `1455`:

```sh
ssh -L 1455:127.0.0.1:1455 apps
```

Then run:

```sh
ssh -t apps \
  "sudo docker run --rm -it --network host \
  -v /data/apps/heka/cliproxy-auth:/root/.cli-proxy-api \
  eceasy/cli-proxy-api:v7.2.100 \
  ./CLIProxyAPI --codex-login --no-browser"
```

CLIProxyAPI watches the authentication directory. Restart its container from
Coolify if a newly added account does not appear immediately.

## Verification

Check Heka liveness:

```sh
curl -fsS http://10.40.0.10:8787/healthz
```

List models through the authenticated OAuth route:

```sh
curl -fsS \
  -H "Authorization: Bearer ${HEKA_TOKEN}" \
  http://10.40.0.10:8787/oauth/v1/models
```

For OpenAI-compatible clients, use:

```text
Base URL: http://10.40.0.10:8787/oauth/v1
API key:  the Heka token
```

The current Heka proxy handles HTTP and SSE. Set WebSocket support to false in
clients that make it optional.

## Logs and status

Both services write to stdout/stderr. Coolify shows their logs separately.
Docker retains up to five 20 MiB local log files per service.

Heka exposes:

- unauthenticated liveness at `GET /healthz`;
- authenticated provider state at `GET /status`;
- authenticated state reset at `POST /status/reset`;
- an authenticated dashboard at `GET /dashboard` — recent requests, errors,
  provider/sidecar state, and the config editor described above.

Request and response bodies are not logged by Heka, and are never written to
disk. They may be held **in memory only**, per route, when that provider or
sidecar's `capture.body` is explicitly enabled in config (default off) —
bounded by `capture.max_bytes` per request and by `history.max_bytes`
overall, visible only through the dashboard, and lost on restart along with
the rest of the in-memory request history. CLIProxyAPI debug and detailed
request logging are disabled.

## Persistence and backup

Two paths must survive redeployments:

- `/data/apps/heka/cliproxy-auth` (OAuth tokens);
- `/data/apps/heka/config` (the live Heka config — now holds plaintext
  provider keys when the dashboard editor is used, same sensitivity class
  as the OAuth token store).

Cuprum includes the containing `apps-data` volume, and both of the above
live under it, in its restic backup.

Traffic is plain HTTP on the trusted private network. Do not expose port
`8787` through NAT or a public interface. Add TLS or a private VPN before
using the service across an untrusted network.
