#!/usr/bin/env bash
# Read-only q3ctl bot-rebalance diagnostic. Run as root on the game host.
# It redacts credentials, player names, addresses, and raw userinfo.
set -euo pipefail

cfg="${Q3CTL_CONFIG_FILE:-/etc/q3ctl/config.json}"
env_file="${Q3CTL_ENV_FILE:-/etc/q3ctl/q3ctl.env}"
endpoint="${Q3CTL_ENDPOINT:-http://127.0.0.1:8088/api/v1/status}"

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  echo "Run with sudo: sudo $0" >&2
  exit 1
fi
for path in "$cfg" "$env_file"; do
  [[ -r "$path" ]] || { echo "Cannot read required q3ctl file: $path" >&2; exit 1; }
done
command -v curl >/dev/null || { echo "curl is required" >&2; exit 1; }
command -v python3 >/dev/null || { echo "python3 is required" >&2; exit 1; }

# shellcheck disable=SC1090
set -a
source "$env_file"
set +a
: "${Q3CTL_ADMIN_PASSWORD:?Missing admin password in q3ctl environment}"

body="$(mktemp)"
trap 'rm -f "$body"' EXIT
admin_user="$(python3 - "$cfg" <<'PY'
import json, sys
with open(sys.argv[1], encoding='utf-8') as fh:
    print(json.load(fh).get('admin_user', ''))
PY
)"
[[ -n "$admin_user" ]] || { echo "q3ctl admin user is missing" >&2; exit 1; }
code="$(curl --silent --show-error --output "$body" --write-out '%{http_code}' --user "$admin_user:$Q3CTL_ADMIN_PASSWORD" "$endpoint")"
printf 'HTTP status: %s\n' "$code"
python3 - "$cfg" "$body" <<'PY'
import json, sys
cfg = json.load(open(sys.argv[1], encoding='utf-8'))
value = json.load(open(sys.argv[2], encoding='utf-8'))
server = value.get('server') or {}
players = server.get('players')
print('live mode: ' + str(server.get('gametype')))
print('live limits: time=%s frag=%s capture=%s' % (server.get('timelimit'), server.get('fraglimit'), server.get('capturelimit')))
print('team readback complete: ' + str((value.get('bot_counts') or {}).get('teams_known')))
if not isinstance(players, list):
    print('player rows: non-list')
    raise SystemExit(0)
counts = {'red': {'human': 0, 'bot': 0}, 'blue': {'human': 0, 'bot': 0}, 'spectator': {'human': 0, 'bot': 0}, 'other': {'human': 0, 'bot': 0}}
for player in players:
    team = player.get('team') if player.get('team') in counts else 'other'
    kind = 'bot' if player.get('bot') else 'human'
    counts[team][kind] += 1
    print('slot %s: %s; team=%s' % (player.get('id'), kind, team))
for team, row in counts.items():
    print('%s totals: humans=%d bots=%d total=%d' % (team, row['human'], row['bot'], row['human'] + row['bot']))
for key in ('game_log_file', 'audit_file'):
    print(key + ': ' + str(cfg.get(key, '')))
PY

echo 'Recent rebalance audit records (redacted):'
python3 - "$cfg" <<'PY'
import json, os, sys
cfg = json.load(open(sys.argv[1], encoding='utf-8'))
path = cfg.get('audit_file', '/var/log/q3ctl/audit.jsonl')
try:
    lines = open(path, encoding='utf-8').read().splitlines()[-40:]
except OSError as exc:
    print('audit unavailable: ' + type(exc).__name__)
    raise SystemExit(0)
for line in lines:
    try:
        record = json.loads(line)
    except Exception:
        continue
    if record.get('action') == 'bot_rebalance':
        print('bot_rebalance: result=' + str(record.get('result', '')))
PY
