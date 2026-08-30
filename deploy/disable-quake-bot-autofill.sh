#!/usr/bin/env bash
# Persistently disable ioquake3's autonomous bot filler.
# Run as root: sudo bash ./disable-quake-bot-autofill.sh
set -euo pipefail

config="${Q3_SERVER_CONFIG:-/usr/lib/ioquake3/baseq3/server.cfg}"

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  echo "Run with sudo: sudo $0" >&2
  exit 1
fi
if [[ ! -f "$config" ]]; then
  echo "Quake server configuration not found: $config" >&2
  exit 1
fi

backup="${config}.q3ctl-bot-autofill.$(date -u +%Y%m%dT%H%M%SZ).bak"
cp -p "$config" "$backup"

python3 - "$config" <<'PY'
from pathlib import Path
import re
import sys

path = Path(sys.argv[1])
text = path.read_text(encoding="utf-8")
pattern = re.compile(r'(?m)^\s*(?:(?:seta|set|sets)\s+)?bot_minplayers\s+"?[0-9]+"?\s*(?:#.*)?$')
replacement = 'seta bot_minplayers "0"'
if pattern.search(text):
    text, count = pattern.subn(replacement, text, count=1)
else:
    text = text.rstrip() + "\n\n// q3ctl owns explicit team bot counts; do not auto-fill.\n" + replacement + "\n"
path.write_text(text, encoding="utf-8")
PY

grep -Eq '^seta bot_minplayers "0"$' "$config"
echo "Disabled Quake automatic bot filling in $config"
echo "Backup: $backup"
echo "No service restart was performed. q3ctl will set the live value when you Save & apply policy or Rebuild bot teams."
