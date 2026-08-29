#!/usr/bin/env bash
# Enable ioquake3's server-side game event log for q3ctl to tail.
# This updates only g_log/g_logsync, makes a dated server.cfg backup, then
# restarts the Quake service. Run as root: sudo bash ./enable-game-log.sh
set -euo pipefail

config=/usr/lib/ioquake3/baseq3/server.cfg
log_file=/usr/lib/ioquake3/baseq3/q3games.log

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  echo "Run with sudo: sudo $0" >&2
  exit 1
fi
if [[ ! -f "$config" ]]; then
  echo "Missing Quake server config: $config" >&2
  exit 1
fi

backup="${config}.q3ctl-game-log.$(date -u +%Y%m%dT%H%M%SZ).bak"
cp -p "$config" "$backup"
python3 - "$config" <<'PY'
from pathlib import Path
import re
import sys

path = Path(sys.argv[1])
text = path.read_text()
settings = {
    "g_log": 'seta g_log "q3games.log"',
    "g_logsync": 'seta g_logsync "1"',
}
for name, line in settings.items():
    pattern = rf'(?im)^\s*(?:set|seta|sets)\s+{re.escape(name)}\s+.*$'
    if re.search(pattern, text):
        text = re.sub(pattern, line, text)
    else:
        text = text.rstrip() + "\n" + line + "\n"
path.write_text(text)
PY

# q3ctl reads this fixed path; quake3 owns the file when it creates it.
rm -f "$log_file"
systemctl restart quake3.service
systemctl is-active --quiet quake3.service

# Trigger a map reload so the cvars are instantiated now, not only after a
# natural rotation. This remains local to the running service.
sleep 2
if [[ -e "$log_file" ]]; then
  echo "Game logging enabled: $log_file"
  ls -l "$log_file"
else
  echo "quake3 is active, but $log_file was not created yet." >&2
  echo "The log will be created on the next map initialization; backup: $backup" >&2
fi
