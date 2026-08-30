#!/usr/bin/env bash
# Read-only q3ctl/Quake connectivity diagnostic. Run as root.
# It never prints passwords or their hashes.
set -euo pipefail

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  echo "Run as root: sudo $0" >&2
  exit 2
fi

config=/etc/q3ctl/config.json
envfile=/etc/q3ctl/q3ctl.env
server_cfg=/usr/lib/ioquake3/baseq3/server.cfg

for required in "$config" "$envfile" "$server_cfg"; do
  [[ -r "$required" ]] || { echo "ERROR: unreadable required file: $required" >&2; exit 1; }
done

. "$envfile"
: "${Q3CTL_RCON_PASSWORD:?missing Q3CTL_RCON_PASSWORD}"
: "${Q3CTL_ADMIN_PASSWORD:?missing Q3CTL_ADMIN_PASSWORD}"

readarray -t cfg < <(python3 - "$config" <<'PY'
import json, sys
cfg = json.load(open(sys.argv[1], encoding="utf-8"))
print(cfg.get("rcon_addr", "127.0.0.1:27960"))
print(cfg.get("listen_addr", "127.0.0.1:8088"))
print(cfg.get("admin_user", ""))
PY
)
rcon_addr=${cfg[0]}
listen_addr=${cfg[1]}
admin_user=${cfg[2]}

if [[ -z "$admin_user" ]]; then
  echo "ERROR: config has no admin_user" >&2
  exit 1
fi

printf 'q3ctl service: '; systemctl is-active q3ctl.service || true
printf 'quake3 service: '; systemctl is-active quake3.service || true
printf 'configured RCON address: %s\n' "$rcon_addr"
printf 'configured q3ctl listener: %s\n' "$listen_addr"
printf 'Quake UDP 27960 listener: '; ss -lun | grep -qE '(^|[[:space:]])0\.0\.0\.0:27960([[:space:]]|$)|(^|[[:space:]])127\.0\.0\.1:27960([[:space:]]|$)' && echo yes || echo no
printf 'q3ctl TCP listener: '; ss -ltn | grep -qF "$listen_addr" && echo yes || echo no

# Check that q3ctl's RCON secret equals the active config's rconPassword without
# exposing either value. Last matching declaration is effective for Quake configs.
python3 - "$server_cfg" "$Q3CTL_RCON_PASSWORD" <<'PY'
import re, sys
cfg = open(sys.argv[1], encoding="utf-8", errors="replace").read()
entries = re.findall(r'^\s*seta?\s+rconPassword\s+"([^"]*)"\s*$', cfg, re.M)
if not entries:
    print("server.cfg RCON password: MISSING")
elif entries[-1] == sys.argv[2]:
    print("server.cfg RCON password: matches q3ctl environment")
else:
    print("server.cfg RCON password: MISMATCH with q3ctl environment")
PY

# Direct RCON test. Only response metadata and public status cvars are emitted.
python3 - "$rcon_addr" "$Q3CTL_RCON_PASSWORD" <<'PY'
import socket, sys
address, password = sys.argv[1:]
host, port = address.rsplit(':', 1)
sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
sock.settimeout(5)
try:
    packet = b'\xff\xff\xff\xffrcon ' + password.encode() + b' status\n'
    sock.sendto(packet, (host, int(port)))
    raw, _ = sock.recvfrom(65535)
except OSError as exc:
    print(f"direct RCON status: FAILED ({exc})")
    raise SystemExit(1)
finally:
    sock.close()
text = raw.decode("utf-8", "replace").replace("\ufffd", "")
if "Bad rconpassword" in text:
    print("direct RCON status: FAILED (Quake rejected the configured password)")
    raise SystemExit(1)
first = next((line for line in text.splitlines() if line.strip()), "<empty response>")
print(f"direct RCON status: OK ({first[:160]})")
PY

# Test the q3ctl API with the configured credentials; do not print credentials.
url="http://${listen_addr}/api/v1/status"
http_code=$(curl --silent --show-error --output /tmp/q3ctl-connectivity-status.json \
  --write-out '%{http_code}' --max-time 10 --user "${admin_user}:${Q3CTL_ADMIN_PASSWORD}" "$url" || true)
if [[ "$http_code" != 2* ]]; then
  echo "q3ctl API status: FAILED (HTTP ${http_code:-no response})"
  head -c 500 /tmp/q3ctl-connectivity-status.json 2>/dev/null || true
  echo
  exit 1
fi

python3 - /tmp/q3ctl-connectivity-status.json <<'PY'
import json, sys
try:
    status=json.load(open(sys.argv[1], encoding='utf-8'))
except Exception as exc:
    print(f"q3ctl API status: FAILED (invalid JSON: {exc})")
    raise SystemExit(1)
if isinstance(status, dict) and status.get('error'):
    print(f"q3ctl API status: FAILED ({status['error']})")
    raise SystemExit(1)
print("q3ctl API status: OK")
print("reported map:", status.get("map", status.get("mapname", "unknown")))
print("reported gametype:", status.get("gametype", status.get("game_type", "unknown")))
PY

echo "RESULT: q3ctl connectivity checks passed"
