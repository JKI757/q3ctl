#!/usr/bin/env bash
# Read-only q3ctl API shape diagnostic. Run as root on the game host.
# It does not print credentials, CSRF tokens, player names, addresses, or API body.
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
admin_user="$(python3 - "$cfg" <<'PY'
import json, sys
with open(sys.argv[1], encoding='utf-8') as fh:
    print(json.load(fh).get('admin_user', ''))
PY
)"
[[ -n "$admin_user" ]] || { echo "q3ctl admin user is missing" >&2; exit 1; }

body="$(mktemp)"
trap 'rm -f "$body"' EXIT
code="$(curl --silent --show-error --output "$body" --write-out '%{http_code}' --user "$admin_user:$Q3CTL_ADMIN_PASSWORD" "$endpoint")"
printf 'HTTP status: %s\n' "$code"
python3 - "$body" <<'PY'
import json, sys
try:
    value = json.load(open(sys.argv[1], encoding='utf-8'))
except Exception as exc:
    print('JSON: invalid (' + type(exc).__name__ + ')')
    raise SystemExit(1)
if not isinstance(value, dict):
    print('JSON root: ' + type(value).__name__)
    raise SystemExit(1)
print('JSON keys: ' + ','.join(sorted(value.keys())))
server = value.get('server')
print('server field: ' + ('null' if server is None else type(server).__name__))
if isinstance(server, dict):
    print('server fields present: map=' + str('map' in server) + ' gametype=' + str('gametype' in server) + ' players=' + str('players' in server))
    players = server.get('players')
    print('player rows: ' + (str(len(players)) if isinstance(players, list) else 'non-list'))
bots = value.get('bot_counts')
print('bot_counts field: ' + ('null' if bots is None else type(bots).__name__))
print('policy field: ' + ('null' if value.get('policy') is None else type(value.get('policy')).__name__))
PY
