#!/usr/bin/env bash
# Upgrade q3ctl from its signed-by-checksum GitHub release asset.
# Run as root: sudo bash ./upgrade-q3ctl.sh [version]
set -euo pipefail

repo="JKI757/q3ctl"
version="${1:-v0.2.23}"
arch="linux-amd64"
base="https://github.com/${repo}/releases/download/${version}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  echo "Run with sudo: sudo $0 [version]" >&2
  exit 1
fi

curl --fail --location --proto '=https' --tlsv1.2 \
  -o "$tmp/q3ctl" "${base}/q3ctl-${arch}"
curl --fail --location --proto '=https' --tlsv1.2 \
  -o "$tmp/SHA256SUMS" "${base}/SHA256SUMS"
expected="$(awk '$2 ~ /q3ctl-linux-amd64$/ {print $1; exit}' "$tmp/SHA256SUMS")"
actual="$(shasum -a 256 "$tmp/q3ctl" | awk '{print $1}')"
if [[ -z "$expected" || "$expected" != "$actual" ]]; then
  echo "checksum verification failed" >&2
  exit 1
fi

install -m 0755 -o root -g root "$tmp/q3ctl" /usr/local/bin/q3ctl
# A locally staged service-unit override is optional; deployments normally
# retain the existing unit to avoid silently changing sandbox policy.
systemctl daemon-reload
systemctl restart q3ctl.service
systemctl is-active --quiet q3ctl.service
ss -ltn | grep -q '127.0.0.1:8088'
echo "q3ctl ${version} installed; systemd service is active on 127.0.0.1:8088."
