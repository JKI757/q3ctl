# q3ctl

`q3ctl` is a loopback-only, authenticated control plane for an ioquake3 dedicated server. It keeps plaintext UDP RCON private and exposes a small, structured web API intended for a private Tailscale or SSH-tunnel path.

## Capabilities

- Live server, player, bot, and game-log visibility.
- Structured map, restart, announcement, and player-kick controls.
- Persistent bot policy and **editable map rotation**.
- Rotation entries define map, gametype, time limit, frag limit, and capture limit.
- **Save rotation** writes an atomically replaced state file. **Apply at next map** safely installs Quake's `d1 … dN` command chain without interrupting the current match.
- SSE streams both the game event log and q3ctl’s audited control actions.

## Layout

```text
cmd/q3ctl/        executable and signal handling
internal/app/     authenticated HTTP API, UI, supervised HTTP lifecycle
internal/config/  non-secret runtime configuration
internal/rcon/    bounded UDP RCON client
internal/store/   atomic state persistence
pkg/q3/           reusable validated Quake domain types and rotation renderer
deploy/            systemd and deployment scripts
```

## Security model

- q3ctl accepts only loopback listeners (`127.0.0.1` / `::1`). Do not publish it with a Lightsail firewall rule.
- Basic-auth and RCON secrets come only from the systemd environment file—never JSON configuration or Git.
- Mutating browser requests require both same-origin validation and a per-process CSRF token.
- The UI has no arbitrary RCON command box. Map names, gametypes, bot names, messages, and rotation limits are validated before commands are rendered.
- State is written with mode `0640` through a temp file + atomic rename.

## Build and verify

```bash
go test ./...
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o dist/q3ctl-linux-amd64 ./cmd/q3ctl
```

## Runtime configuration

`/etc/q3ctl/config.json` contains only non-secrets:

```json
{
  "listen": "127.0.0.1:8088",
  "rcon_addr": "127.0.0.1:27960",
  "admin_user": "admin",
  "state_file": "/var/lib/q3ctl/state.json",
  "audit_file": "/var/log/q3ctl/audit.jsonl",
  "game_log_file": "/var/log/q3ctl/game.log"
}
```

`/etc/q3ctl/q3ctl.env` must be `root:q3ctl 0640` and contain:

```bash
Q3CTL_ADMIN_PASSWORD=<long-random-password>
Q3CTL_RCON_PASSWORD=<matching-Quake-rcon-password>
```

## Rotation behavior

A saved rotation is durable in `/var/lib/q3ctl/state.json`. Applying it renders the same looping `d1…dN` pattern as the server’s stock configuration and sets `nextmap` to `vstr d1`. It does **not** load a map, restart the service, or cut off the current match. The first edited rotation map begins at the next map transition.

## API highlights

- `GET /health`
- `GET /api/v1/status`
- `GET|PUT /api/v1/rotation`
- `POST /api/v1/rotation/apply`
- `GET /api/v1/logs/stream`

All mutating endpoints require Basic authentication, same-origin validation, and `X-CSRF-Token` supplied by the authenticated dashboard.
