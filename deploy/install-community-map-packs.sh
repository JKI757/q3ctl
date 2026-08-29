#!/usr/bin/env bash
# Install verified non-TA community map-pack archives into stock baseq3.
# Run as root: sudo ./install-community-map-packs.sh /path/to/*.zip
set -euo pipefail

readonly target_dir=/usr/lib/ioquake3/baseq3
declare -Ar expected_sha256=(
  [lvl_10th_anniversary_ctf.zip]=7bbd6cf8dbdcfb0f0b88c3db521ca9b7cad4c17821a33aa392c078d883aeaa1f
  [lvl_10th_anniversary_tdm.zip]=e0e2facc270da2e2e0848e947653c1baa2345c1a2318d5f79146061b5437bd29
  [actf_maps1.zip]=18d5c3faede5023524d3a67c7808b5a94ac7035bea37113a8af8c1d1a7779438
  [actf_maps2.zip]=2a040e9613e5ebf89c3fbaa2820b37d2c485b31f19e8fd35ae72d6244c76c161
)

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  echo "Run as root: sudo $0 /path/to/*.zip" >&2
  exit 1
fi
if [[ $# -eq 0 ]]; then
  echo "usage: sudo $0 /path/to/{lvl_10th_anniversary_ctf,lvl_10th_anniversary_tdm,actf_maps1,actf_maps2}.zip" >&2
  exit 1
fi
if [[ ! -d "$target_dir" ]]; then
  echo "baseq3 directory not found: $target_dir" >&2
  exit 1
fi

for source in "$@"; do
  name=$(basename "$source")
  expected=${expected_sha256[$name]:-}
  if [[ -z "$expected" ]]; then
    echo "unrecognized map archive: $name" >&2
    exit 1
  fi
  if [[ ! -r "$source" ]]; then
    echo "cannot read source archive: $source" >&2
    exit 1
  fi
  actual=$(sha256sum "$source" | cut -d' ' -f1)
  if [[ "$actual" != "$expected" ]]; then
    echo "checksum mismatch; refusing $name" >&2
    exit 1
  fi
done

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
python3 - "$stage" "$@" <<'PY'
import hashlib, pathlib, shutil, sys, zipfile
stage = pathlib.Path(sys.argv[1])
for archive_name in sys.argv[2:]:
    archive = pathlib.Path(archive_name)
    with zipfile.ZipFile(archive) as outer:
        for entry in outer.infolist():
            path = pathlib.PurePosixPath(entry.filename)
            if path.is_absolute() or '..' in path.parts:
                raise SystemExit(f'unsafe path in {archive.name}: {entry.filename}')
            if not entry.is_dir() and entry.filename.lower().endswith('.pk3'):
                destination = stage / path.name
                with outer.open(entry) as source, destination.open('wb') as output:
                    shutil.copyfileobj(source, output)
package_paths = sorted(stage.glob('*.pk3'))
map_count = 0
for package in package_paths:
    with zipfile.ZipFile(package) as pk3:
        names = [entry.filename.lower() for entry in pk3.infolist()]
        if any(name.startswith('vm/') or name.endswith('.qvm') for name in names):
            raise SystemExit(f'executable Q3 VM found in {package.name}')
        maps = [name for name in names if name.startswith('maps/') and name.endswith('.bsp')]
        map_count += len(maps)
        # Asset-only PK3s are allowed because some packs split shared textures,
        # sounds, or scripts from their map BSPs.
        print(f'{package.name}: {len(maps)} maps, SHA256 {hashlib.sha256(package.read_bytes()).hexdigest()}')
if not package_paths:
    raise SystemExit('no PK3 files found in supplied archives')
if not map_count:
    raise SystemExit('no BSP maps found in supplied archives')
PY

# Copy only after every source archive and every embedded PK3 passed validation.
install -m 0644 -o root -g root "$stage"/*.pk3 "$target_dir/"
python3 - "$target_dir/server.cfg" <<'PY'
import re, sys
path = sys.argv[1]
text = open(path, encoding='utf-8').read()
line = 'seta sv_allowDownload "1"'
pattern = r'^\s*(?:set|seta)\s+sv_allowDownload\s+"?[01]"?\s*$'
if re.search(pattern, text, re.M):
    text = re.sub(pattern, line, text, flags=re.M)
else:
    text += '\n// Allow native client download of server maps/models\n' + line + '\n'
open(path, 'w', encoding='utf-8').write(text)
PY
chmod 0644 "$target_dir/server.cfg"
systemctl restart quake3.service
systemctl is-active --quiet quake3.service
printf 'Installed verified community map packages; native server downloads enabled; quake3.service is active.\n'
