# q3ctl

`q3ctl` is a deliberately local-only Go control plane for the ioquake3 server on C62.

## Security model

- Quake 3 UDP RCON is plaintext and is **never exposed directly**.
- q3ctl rejects non-loopback listeners. Reach it remotely through a private VPN/Tailscale or SSH SOCKS/HTTP tunnel, not a Lightsail firewall rule.
- The HTTP API requires Basic authentication. Store its password and the RCON password in a root-owned systemd environment file, never in the JSON config or Git.
- q3ctl accepts only a small RCON allowlist; it does not provide arbitrary shell access.

## What it manages

- authenticated server status and dashboard;
- selected RCON controls: maps, announcements, bots, map restart, kicks;
- a validated bot policy API: humans on a configured team; bots on both teams; 1–5 difficulty bounds; adaptive-policy settings.

The current release includes live player/status parsing, a safe bot-roster reconcile action, map/mode selection, persisted bot policy and rotation settings, player kick and announcements, plus a streaming audit log. **It does not force-move human clients:** stock Quake 3 requires a game mod for that capability. Bot reconciliation is explicitly operator-triggered so it does not disrupt a live match.

## Build

```bash
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o dist/q3ctl-linux-amd64 .
```

## Required server files

`/etc/q3ctl/config.json` (root:root, `0644`):

```json
{"listen":"127.0.0.1:8088","rcon_addr":"127.0.0.1:27960","admin_user":"josh"}
```

`/etc/q3ctl/q3ctl.env` (root:q3ctl, `0640`):

```bash
Q3CTL_ADMIN_PASSWORD=<long-random-password>
Q3CTL_RCON_PASSWORD=<long-random-rcon-password>
```

Set the same RCON password in the Quake configuration as `seta rconPassword "..."`. The controller itself runs as an unprivileged `q3ctl` system user.

## HTTP API

- `GET /health`
- `GET /api/status`
- `GET|PUT /api/policy`
- `POST /api/rcon` with `{"command":"map q3ctf2"}`

The homepage is a minimal authenticated dashboard.
