#!/usr/bin/env bash
# Install the verified OSP Maps 0 asset pack for stock baseq3 (no Team Arena).
# Run as root: sudo ./install-ospmaps0.sh /path/to/ospmaps0.pk3
set -euo pipefail

source=${1:?usage: sudo ./install-ospmaps0.sh /path/to/ospmaps0.pk3}
target_dir=/usr/lib/ioquake3/baseq3
target="$target_dir/ospmaps0.pk3"
expected=b5dacaf7f1203c43e5baabb8b3824b00330e7d5a6c620626f883300b67c9b748

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  echo "Run as root: sudo $0 /path/to/ospmaps0.pk3" >&2
  exit 1
fi
if [[ ! -r "$source" ]]; then
  echo "cannot read source package: $source" >&2
  exit 1
fi
if [[ ! -d "$target_dir" ]]; then
  echo "baseq3 directory not found: $target_dir" >&2
  exit 1
fi
actual=$(sha256sum "$source" | awk '{print $1}')
if [[ "$actual" != "$expected" ]]; then
  echo "OSP package checksum mismatch; refusing install" >&2
  exit 1
fi

# Validate before the privileged copy: package must contain the expected maps,
# bot AAS navigation, and no executable Q3 VM modules.
python3 - "$source" <<'PY'
import sys, zipfile
p = sys.argv[1]
expected = {
    'ospca1', 'ospctf1', 'ospctf2', 'ospdm1', 'ospdm2', 'ospdm3', 'ospdm4',
    'ospdm5', 'ospdm6', 'ospdm7', 'ospdm8', 'ospdm9', 'ospdm10', 'ospdm11', 'ospdm12',
}
with zipfile.ZipFile(p) as z:
    names = set(z.namelist())
    maps = {n[5:-4].lower() for n in names if n.lower().startswith('maps/') and n.lower().endswith('.bsp')}
    modules = [n for n in names if n.lower().startswith('vm/') or n.lower().endswith('.qvm')]
missing = expected - maps
if missing or modules or any('maps/' + name + '.aas' not in names for name in expected):
    raise SystemExit('package content validation failed')
PY

install -m 0644 -o root -g root "$source" "$target"
# Native ioquake3 downloads let clean clients obtain the exact server assets.
python3 - "$target_dir/server.cfg" <<'PY'
import re, sys
p = sys.argv[1]
s = open(p, encoding='utf-8').read()
line = 'seta sv_allowDownload "1"'
pattern = r'^\s*(?:set|seta)\s+sv_allowDownload\s+"?[01]"?\s*$'
if re.search(pattern, s, re.M):
    s = re.sub(pattern, line, s, flags=re.M)
else:
    s += '\n// Allow native client download of server maps/models\n' + line + '\n'
open(p, 'w', encoding='utf-8').write(s)
PY
chmod 0644 "$target_dir/server.cfg"

systemctl restart quake3.service
systemctl is-active --quiet quake3.service
printf 'Installed %s; native server downloads enabled; quake3.service is active.\n' "$target"
