#!/usr/bin/env bash
# Allow the unprivileged q3ctl catalog reader to inspect public baseq3 PK3 assets.
# Run as root: sudo ./repair-q3ctl-map-catalog-permissions.sh
set -euo pipefail

readonly baseq3=/usr/lib/ioquake3/baseq3

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  echo "Run as root: sudo $0" >&2
  exit 1
fi
if [[ ! -d "$baseq3" ]]; then
  echo "baseq3 directory not found: $baseq3" >&2
  exit 1
fi

# PK3 files contain public client game assets. q3ctl needs read—not write—access
# to build its installed-map catalog. Restrict the change to the stock archives.
shopt -s nullglob
stock=("$baseq3"/pak[0-9].pk3)
if [[ ${#stock[@]} -eq 0 ]]; then
  echo "no stock pakN.pk3 archives found in $baseq3" >&2
  exit 1
fi
chmod a+r "${stock[@]}"

for pak in "${stock[@]}"; do
  [[ -r "$pak" ]] || { echo "could not make readable: $pak" >&2; exit 1; }
done

systemctl restart q3ctl.service
systemctl is-active --quiet q3ctl.service
printf 'Made %d stock PK3 archives readable and restarted q3ctl.service.\n' "${#stock[@]}"
