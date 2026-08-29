#!/usr/bin/env bash
# Make ioquake3 game events visible to q3ctl without weakening q3ctl's
# ProtectHome sandbox. Run as root: sudo bash repair-game-log-bridge.sh
set -euo pipefail

quake_cfg=/usr/lib/ioquake3/baseq3/server.cfg
quake_log=/home/quake3/.q3a/baseq3/q3games.log
q3ctl_cfg=/etc/q3ctl/config.json
bridge_log=/var/log/q3ctl/game.log
bridge_unit=/etc/systemd/system/q3-game-log-bridge.service

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  echo "Run with sudo: sudo $0" >&2
  exit 1
fi
for command in systemctl python3 install tail; do
  command -v "$command" >/dev/null || { echo "Missing required command: $command" >&2; exit 1; }
done
for file in "$quake_cfg" "$q3ctl_cfg"; do
  [[ -f "$file" ]] || { echo "Missing required file: $file" >&2; exit 1; }
done

backup_cfg="${quake_cfg}.q3ctl-game-log-bridge.$(date -u +%Y%m%dT%H%M%SZ).bak"
cp -p "$quake_cfg" "$backup_cfg"
python3 - "$quake_cfg" "$q3ctl_cfg" "$bridge_log" <<'PY'
from pathlib import Path
import json, re, sys
quake_cfg, q3ctl_cfg, bridge_log = map(Path, sys.argv[1:])
text = quake_cfg.read_text()
settings = {
    "g_log": 'seta g_log "q3games.log"',
    "g_logsync": 'seta g_logsync "1"',
}
for name, line in settings.items():
    pattern = rf'(?im)^\s*(?:set|seta|sets)\s+{re.escape(name)}\s+.*$'
    text = re.sub(pattern, line, text) if re.search(pattern, text) else text.rstrip() + "\n" + line + "\n"
quake_cfg.write_text(text)
config = json.loads(q3ctl_cfg.read_text())
config["game_log_file"] = str(bridge_log)
q3ctl_cfg.write_text(json.dumps(config, indent=2) + "\n")
PY

# The Quake service owns its homepath; q3ctl remains unable to access /home.
install -d -o quake3 -g quake3 -m 0750 /home/quake3/.q3a/baseq3
touch "$quake_log"
chown quake3:quake3 "$quake_log"
chmod 0644 "$quake_log"

# q3ctl already has this location in ReadWritePaths. The setgid directory
# ensures the bridge's output is readable by the q3ctl service group.
install -d -o quake3 -g q3ctl -m 2770 /var/log/q3ctl
rm -f "$bridge_log"

cat >"$bridge_unit" <<EOF
[Unit]
Description=Copy ioquake3 game events into q3ctl's permitted log directory
After=quake3.service
Requires=quake3.service

[Service]
Type=simple
User=quake3
Group=q3ctl
UMask=0027
ExecStart=/bin/sh -c 'exec /usr/bin/tail -n 1000 -F ${quake_log} >> ${bridge_log}'
Restart=always
RestartSec=1
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
# The bridge needs read-only access to Quake's own homepath. q3ctl itself
# remains separately sandboxed with ProtectHome=true.
ProtectHome=read-only
ReadWritePaths=/var/log/q3ctl

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl restart quake3.service
sleep 2
if [[ ! -s "$quake_log" ]]; then
  echo "Quake did not write an InitGame event to $quake_log after restart." >&2
  echo "Restored config backup is available at: $backup_cfg" >&2
  exit 1
fi
systemctl enable --now q3-game-log-bridge.service
systemctl restart q3ctl.service
systemctl is-active --quiet quake3.service
systemctl is-active --quiet q3-game-log-bridge.service
systemctl is-active --quiet q3ctl.service
sleep 1

echo "Game-log bridge is active."
printf 'Quake source: '; stat -c '%U:%G %a %s %n' "$quake_log"
printf 'q3ctl stream: '; stat -c '%U:%G %a %s %n' "$bridge_log"
tail -n 10 "$bridge_log"
