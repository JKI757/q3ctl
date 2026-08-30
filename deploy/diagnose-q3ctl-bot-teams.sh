#!/usr/bin/env bash
# Read-only ioquake3 bot/team diagnostics. Run as root on the game host.
# It never prints RCON credentials, player names, IPs, or full userinfo data.
set -euo pipefail

ENV_FILE="${Q3CTL_ENV_FILE:-/etc/q3ctl/q3ctl.env}"
CONFIG_FILE="${Q3CTL_CONFIG_FILE:-/etc/q3ctl/config.json}"

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  echo "Run with sudo: sudo $0" >&2
  exit 1
fi
[[ -r "$ENV_FILE" ]] || { echo "Cannot read q3ctl environment: $ENV_FILE" >&2; exit 1; }
[[ -r "$CONFIG_FILE" ]] || { echo "Cannot read q3ctl config: $CONFIG_FILE" >&2; exit 1; }

# shellcheck disable=SC1090
set -a
source "$ENV_FILE"
set +a
: "${Q3CTL_RCON_PASSWORD:?Q3CTL_RCON_PASSWORD is required in $ENV_FILE}"

RCON_ADDR="$(python3 - "$CONFIG_FILE" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as fh:
    print(json.load(fh).get("rcon_addr") or "127.0.0.1:27960")
PY
)"

Q3CTL_RCON_ADDR="$RCON_ADDR" Q3CTL_RCON_PASSWORD="$Q3CTL_RCON_PASSWORD" python3 - <<'PY'
import os, re, socket

addr = os.environ["Q3CTL_RCON_ADDR"]
password = os.environ["Q3CTL_RCON_PASSWORD"]
host, port_text = addr.rsplit(":", 1)
port = int(port_text)
prefix = b"\xff\xff\xff\xff"

def rcon(command):
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    sock.settimeout(2.5)
    try:
        sock.sendto(prefix + b"rcon " + password.encode() + b" " + command.encode() + b"\n", (host, port))
        data, _ = sock.recvfrom(16384)
        return data.removeprefix(prefix + b"print\n").decode("latin1", "replace"), None
    except OSError as exc:
        return "", exc
    finally:
        sock.close()

print("q3ctl bot/team diagnostic (read-only)")
for command in ("g_gametype", "bot_minplayers", "g_teamAutoJoin", "g_teamForceBalance"):
    reply, err = rcon(command)
    outcome = "timeout" if err else "reply"
    # Only cvar command and whether a reply arrived; cvar values are parsed below.
    value = "unknown"
    if not err:
        m = re.search(r'\bis\s*:?\s*"?([^"\r\n]*)', reply)
        if m:
            value = m.group(1).replace("^7", "").strip()
    print(f"{command}: {value} ({outcome})")

status, err = rcon("status")
if err:
    print(f"status: failed ({type(err).__name__})")
    raise SystemExit(1)

# ioquake3's RCON status table is fixed-column: address comes after the
# padded 16-character name and before the final numeric rate. Do not mistake
# the final rate for an address; that was a diagnostic bug in the prior script.
client_ids = []
for line in status.replace("\r", "").split("\n"):
    match = re.match(r'^\s*(\d+)\s+[-\d]+\s+(?:[-\d]+|CON|ZMB)\s+.+?\s{2,}(.+?)\s+\d+\s*$', line)
    if not match:
        continue
    client_ids.append((int(match.group(1)), "bot" in match.group(2).lower()))
print(f"status rows: {len(client_ids)}")
print(f"bots by engine status address: {sum(is_bot for _, is_bot in client_ids)}")
for client_id, is_bot in client_ids:
    reply, err = rcon(f"dumpuser {client_id}")
    keys = "unavailable"
    if not err:
        # dumpuser exposes client-provided userinfo, not authoritative game-team
        # state. Print only the field names, never values (which include names).
        info_start = reply.find("\\")
        if info_start >= 0:
            fields = reply[info_start + 1:].split("\\")
            keys = ",".join(sorted(set(fields[::2]))) or "none"
    print(f"client {client_id}: {'bot' if is_bot else 'human'}; dumpuser_keys={keys}; dumpuser={'timeout' if err else 'reply'}")
PY
