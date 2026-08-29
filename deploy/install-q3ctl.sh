#!/usr/bin/env bash
# One-time privileged deployment for q3ctl. Run interactively as root:
#   sudo /home/josh/q3ctl-staging/install-q3ctl.sh
set -euo pipefail

STAGE=/home/josh/q3ctl-staging
GAME=/usr/lib/ioquake3/baseq3
install -d -m 0750 -o root -g q3ctl /etc/q3ctl 2>/dev/null || true
if ! id q3ctl >/dev/null 2>&1; then
  useradd --system --home /var/lib/q3ctl --shell /usr/sbin/nologin q3ctl
fi
install -d -m 0750 -o q3ctl -g q3ctl /var/lib/q3ctl /var/log/q3ctl
install -d -m 0750 -o root -g q3ctl /etc/q3ctl
install -m 0755 -o root -g root "$STAGE/q3ctl-linux-amd64" /usr/local/bin/q3ctl
install -m 0644 -o root -g root "$STAGE/q3ctl.service" /etc/systemd/system/q3ctl.service
install -m 0644 -o root -g root "$STAGE/config.json" /etc/q3ctl/config.json

# Generate secrets only once; never print them. q3ctl's group needs read-only access.
if [[ ! -f /etc/q3ctl/q3ctl.env ]]; then
  admin=$(openssl rand -base64 36 | tr -d '\n')
  rcon=$(openssl rand -base64 36 | tr -d '\n')
  umask 027
  printf 'Q3CTL_ADMIN_PASSWORD=%q\nQ3CTL_RCON_PASSWORD=%q\n' "$admin" "$rcon" > /etc/q3ctl/q3ctl.env
  chown root:q3ctl /etc/q3ctl/q3ctl.env
  chmod 0640 /etc/q3ctl/q3ctl.env

  # Replace an existing rcon setting or append it. Keep cfg quake3-readable.
  python3 - "$GAME/server.cfg" "$rcon" <<'PY'
import re, sys
p, password = sys.argv[1:]
s = open(p).read()
line = f'seta rconPassword "{password}"'
if re.search(r'^\s*seta\s+rconPassword\s+"[^"]*"\s*$', s, re.M):
    s = re.sub(r'^\s*seta\s+rconPassword\s+"[^"]*"\s*$', line, s, flags=re.M)
else:
    s += '\n// q3ctl server-local RCON credential\n' + line + '\n'
open(p, 'w').write(s)
PY
  chown quake3:quake3 "$GAME/server.cfg"
  chmod 0644 "$GAME/server.cfg"
fi

systemctl daemon-reload
systemctl restart quake3.service
systemctl enable --now q3ctl.service
systemctl is-active --quiet quake3.service
systemctl is-active --quiet q3ctl.service
curl -fsS -o /dev/null http://127.0.0.1:8088/health || true # authentication intentionally returns 401
printf 'q3ctl installed and listening on 127.0.0.1:8088.\n'
printf 'Next: install/enroll Tailscale, then use tailscale serve to proxy HTTPS to q3ctl.\n'
