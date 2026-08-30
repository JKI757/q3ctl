#!/usr/bin/env bash
# Diagnose a q3ctl map-load request using the exact locally configured RCON secret.
# Run as root: sudo ./diagnose-q3ctl-map-load.sh MAP GAMETYPE
set -euo pipefail

map=${1:?usage: sudo ./diagnose-q3ctl-map-load.sh MAP GAMETYPE}
gametype=${2:?usage: sudo ./diagnose-q3ctl-map-load.sh MAP GAMETYPE}
case "$map" in
  *[!a-zA-Z0-9_-]*|'') echo "invalid map name" >&2; exit 2 ;;
esac
case "$gametype" in 0|1|3|4) ;; *) echo "gametype must be 0, 1, 3, or 4" >&2; exit 2 ;; esac

set -a
# shellcheck disable=SC1091
. /etc/q3ctl/q3ctl.env
set +a
rcon_addr=$(python3 - <<'PY'
import json
print(json.load(open('/etc/q3ctl/config.json'))['rcon_addr'])
PY
)
python3 - "$rcon_addr" "$Q3CTL_RCON_PASSWORD" "$map" "$gametype" <<'PY'
import socket, sys, time
address, password, map_name, game_type = sys.argv[1:]
host, port = address.rsplit(':', 1)
port = int(port)
def rcon(command):
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as s:
        s.settimeout(3)
        s.sendto(b'\xff\xff\xff\xffrcon ' + password.encode() + b' ' + command.encode() + b'\n', (host, port))
        return s.recvfrom(16384)[0].decode('utf-8', 'replace').replace('\xff\xff\xff\xffprint\n', '', 1)
print('before:', rcon('status').splitlines()[0])
print('load reply:', rcon(f'set g_gametype {game_type}; map {map_name}').strip())
for _ in range(12):
    time.sleep(.25)
    first = rcon('status').splitlines()[0]
    print('status:', first)
    if f'\\mapname\\{map_name}\\' in first and f'\\g_gametype\\{game_type}\\' in first:
        print('CONFIRMED')
        break
else:
    raise SystemExit('UNCONFIRMED: Quake did not report the requested map/mode')
PY
