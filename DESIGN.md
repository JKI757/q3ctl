# q3ctl — Quake 3 server control plane

## Goal

A self-hosted authenticated dashboard and JSON API for a Quake 3 server. It manages **only** declared game-server operations; it is not a remote shell and it never exposes Quake 3's plaintext UDP RCON on the public Internet.

## Deployment topology

```mermaid
flowchart LR
  Browser[Phone / browser] -->|private encrypted access| Private[VPN / private tunnel]
  Private -->|HTTPS| Q[q3ctl :8088]
  Q -->|loopback UDP RCON| Q3[ioq3ded :27960]
  Q -->|narrow sudo allowlist| Systemd[systemctl quake3.service]
  Q --> State[/var/lib/q3ctl/state.json]
  Q --> Log[/var/log/q3ctl]
```

- `q3ctl` runs as its own unprivileged Linux user.
- It binds **only** `127.0.0.1:8088`.
- A private-access layer is required for remote use. The recommended choice is Tailscale; it adds encrypted identity-aware access without opening a Lightsail port.
- A root-owned systemd unit starts q3ctl. A tightly scoped sudoers entry permits exactly `status`, `start`, `stop`, and `restart` for `quake3.service`.
- Quake 3 remains bound to UDP `27960`; management commands are sent only to `127.0.0.1:27960`.

## Security requirements

1. **No public RCON and no public dashboard listener.** RCON puts its password on the wire.
2. q3ctl gets a long random admin password and RCON password from a root-owned environment file (`0640`, group `q3ctl`).
3. Any user-writable passwordless-sudo script must be removed before q3ctl deployment. A user-writable script authorized by sudo is equivalent to full root.
4. API is authenticated; future production UI adds CSRF protection and rate limiting before any Internet-facing proxy is considered.
5. Operations are allowlisted and structured. There is no arbitrary RCON text box in the production dashboard.
6. Audit every mutating request: authenticated principal, request, result, and correlation ID—never passwords.

## Features

### Server lifecycle

- server/service health and version;
- start, stop, restart;
- recent service logs;
- process, memory, uptime, UDP listener, player count;
- safe configuration reload/restart with server-up verification.

### Maps and rotation

- installed map inventory derived from `.pk3` archives;
- one-click next map / map restart;
- editable named rotations, each entry declaring map, gametype, time/frag/capture limits;
- validate map availability before saving or applying;
- save the structured definition atomically to q3ctl state;
- apply a validated `d1…dN` next-map chain over server-local RCON without restarting or interrupting the active match;
- record the apply action and confirm the selected map/mode after the next map transition.

### Bots and teams

- enabled state, target bots per team, bot roster, difficulty 1–5;
- `all humans on red|blue`, with bots filling both teams;
- selectable alternate modes: normal team balance, humans split, bots-only;
- policy applies only at map start, intermission, or an operator-chosen safe point—not during an active fight;
- player status model separates connected players, inferred bots, human team, and score/ping;
- automatic difficulty controller evaluates a moving window of completed team rounds:
  - target competitive band: human team win rate 45–55%;
  - after 3 decisive human losses, lower opposing skill by 1; after 3 decisive human wins, raise opposing skill by 1;
  - lower the friendly side first on imbalance; never change outside configured 1–5 bounds;
  - every change appears in the audit log and can be disabled or overridden.

### Admin actions

- announce message;
- kick client, kick all bots, ban/unban with an explicit confirmation UX;
- match/map restart with countdown;
- RCON status and health diagnostics.

## API (v1)

- `GET /health`
- `GET /api/v1/status`
- `GET /api/v1/service`
- `POST /api/v1/service/{start|stop|restart}`
- `GET|PUT /api/v1/rotation`
- `POST /api/v1/rotation/apply`
- `POST /api/v1/maps/{name}/load`
- `POST /api/v1/maps/restart`
- `GET|PUT /api/v1/bots/policy`
- `POST /api/v1/bots/reconcile`
- `GET /api/v1/players`
- `POST /api/v1/players/{id}/kick`
- `POST /api/v1/announce`
- `GET /api/v1/audit`

All mutating routes require authentication and a CSRF token in the browser UI.

## Delivery order

1. **Foundation:** deploy local-only q3ctl service; authenticated status/map inventory; fixed RCON secret; audit log.
2. **Configuration:** rotation CRUD/apply/rollback and systemd lifecycle controls.
3. **Bots:** collect live RCON samples, prove bot-vs-human detection, then implement safe-point team reconciliation and static roster policy.
4. **Adaptive director:** add match statistics persistence and bounded skill adjustments; verify manually against several maps.
5. **Remote UI:** add Tailscale and publish q3ctl only on the tailnet; no Lightsail management port.

## Acceptance tests

- Requests without auth receive `401`; dashboard remains loopback-only.
- A rotation with a nonexistent map or unsafe limit is rejected without modifying persisted state or issuing RCON.
- Saving a valid rotation atomically replaces q3ctl state; applying it updates only Quake's next-map chain and does not restart or interrupt the current match.
- In a CTF test, two human players are placed on the configured side and configured bots fill both sides only at a safe point.
- Adaptive controller changes skill only after configured evidence, never beyond 1–5, and records why.
- Every lifecycle operation reports final systemd state; failure does not claim success.
