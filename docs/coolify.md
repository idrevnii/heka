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

Configuration without secrets lives in:

- `deploy/heka.yaml` for Heka routes and rotation policy;
- `docker-compose.yml` under `configs.cliproxy-config` for CLIProxyAPI.

Commit configuration changes and redeploy the application. Heka reads its
configuration at startup and currently requires a restart after changes.

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
- authenticated state reset at `POST /status/reset`.

Request and response bodies are not logged by Heka. CLIProxyAPI debug and
detailed request logging are disabled.

## Persistence and backup

The only state that must survive redeployments is
`/data/apps/heka/cliproxy-auth`. Cuprum includes the containing `apps-data`
volume in its restic backup.

Traffic is plain HTTP on the trusted private network. Do not expose port
`8787` through NAT or a public interface. Add TLS or a private VPN before
using the service across an untrusted network.
