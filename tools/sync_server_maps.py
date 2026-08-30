#!/usr/bin/env python3
"""Download verified community PK3s from a Quake III server into local baseq3.

Requires the system OpenSSH clients (ssh and scp), available by default on
macOS and most Linux distributions. On Windows, install the OpenSSH Client
optional feature or run it from a terminal that provides ssh.exe/scp.exe.

The script deliberately excludes the licensed stock pak*.pk3 archives. It
fetches a fresh SHA-256 manifest from the server, downloads every non-stock
PK3, verifies it, then atomically installs it in the local baseq3 directory.
"""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import os
from pathlib import Path
import shutil
import subprocess
import sys
from typing import Iterable

REMOTE_BASEQ3 = "/usr/lib/ioquake3/baseq3"
DEFAULT_REMOTE = "josh@100.100.96.1"  # C62's Tailscale address.


def candidates() -> list[Path]:
    home = Path.home()
    values = [
        # macOS Steam library defaults and common manual app locations.
        home / "Library/Application Support/Steam/steamapps/common/Quake III Arena/baseq3",
        home / "Library/Application Support/Steam/steamapps/common/Quake 3 Arena/baseq3",
        home / "Library/Application Support/Steam/steamapps/common/Quake III Arena/ioquake3/baseq3",
        Path("/Applications/Quake III Arena.app/Contents/Resources/baseq3"),
        Path("/Applications/ioquake3/baseq3"),
        # Linux Steam library defaults.
        home / ".steam/steam/steamapps/common/Quake III Arena/baseq3",
        home / ".local/share/Steam/steamapps/common/Quake III Arena/baseq3",
        # Windows Steam defaults; pathlib handles these when run on Windows.
        Path(os.environ.get("PROGRAMFILES(X86)", r"C:\\Program Files (x86)")) / "Steam/steamapps/common/Quake III Arena/baseq3",
        Path(os.environ.get("PROGRAMFILES", r"C:\\Program Files")) / "Steam/steamapps/common/Quake III Arena/baseq3",
    ]
    return values


def find_game_dir() -> Path | None:
    for path in candidates():
        if path.is_dir() and any(path.glob("pak0.pk3")):
            return path
    return None


def executable(name: str) -> str:
    value = shutil.which(name)
    if not value:
        raise RuntimeError(f"{name!r} was not found on PATH; install/enable the OpenSSH client first")
    return value


def run(command: list[str], *, input_text: str | None = None) -> subprocess.CompletedProcess[str]:
    return subprocess.run(command, input=input_text, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=True)


def ssh_base(identity: Path | None, port: int) -> list[str]:
    command = [executable("ssh"), "-o", "BatchMode=yes", "-o", "ConnectTimeout=20", "-p", str(port)]
    if identity:
        command += ["-i", str(identity.expanduser())]
    return command


def scp_base(identity: Path | None, port: int) -> list[str]:
    command = [executable("scp"), "-o", "BatchMode=yes", "-o", "ConnectTimeout=20", "-P", str(port)]
    if identity:
        command += ["-i", str(identity.expanduser())]
    return command


def remote_manifest(remote: str, identity: Path | None, port: int) -> dict[str, str]:
    # Names are emitted separately from hashes so only direct files in baseq3
    # are considered. pak*.pk3 is intentionally excluded: it is licensed game
    # data that should come from the player's own Steam/GOG/CD installation.
    script = f'''set -euo pipefail
base={REMOTE_BASEQ3!r}
find "$base" -maxdepth 1 -type f -name '*.pk3' ! -name 'pak*.pk3' -printf '%f\\n' | LC_ALL=C sort | while IFS= read -r name; do
  sha256sum "$base/$name" | awk -v name="$name" '{{print $1 "\\t" name}}'
done
'''
    result = run(ssh_base(identity, port) + [remote, "bash", "-s"], input_text=script)
    manifest: dict[str, str] = {}
    for line in result.stdout.splitlines():
        digest, separator, name = line.partition("\t")
        if not separator or len(digest) != 64 or not name.endswith(".pk3") or Path(name).name != name:
            raise RuntimeError(f"unexpected manifest line from server: {line!r}")
        manifest[name] = digest.lower()
    if not manifest:
        raise RuntimeError("server returned no community PK3 files")
    return dict(sorted(manifest.items()))


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def install_one(name: str, expected: str, target: Path, remote: str, identity: Path | None, port: int, dry_run: bool) -> str:
    destination = target / name
    if destination.is_file() and sha256(destination) == expected:
        return "already verified"
    if dry_run:
        return "would download"

    temporary = target / f".{name}.download"
    temporary.unlink(missing_ok=True)
    remote_path = f"{remote}:{REMOTE_BASEQ3}/{name}"
    try:
        run(scp_base(identity, port) + [remote_path, str(temporary)])
        actual = sha256(temporary)
        if actual != expected:
            raise RuntimeError(f"SHA-256 mismatch for {name}: got {actual}, expected {expected}")
        if destination.exists():
            backup_dir = target / "q3ctl-map-backups" / dt.datetime.now().strftime("%Y%m%d-%H%M%S")
            backup_dir.mkdir(parents=True, exist_ok=True)
            shutil.move(str(destination), str(backup_dir / name))
            print(f"  backed up differing local {name} to {backup_dir}")
        os.replace(temporary, destination)
        return "downloaded and verified"
    finally:
        temporary.unlink(missing_ok=True)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--game-dir", type=Path, help="local baseq3 directory; auto-detected when omitted")
    parser.add_argument("--remote", default=DEFAULT_REMOTE, help=f"SSH remote (default: {DEFAULT_REMOTE})")
    parser.add_argument("--identity", type=Path, help="SSH private-key path, if needed")
    parser.add_argument("--port", type=int, default=22, help="SSH port (default: 22)")
    parser.add_argument("--dry-run", action="store_true", help="list files without downloading")
    args = parser.parse_args()

    target = args.game_dir.expanduser() if args.game_dir else find_game_dir()
    if target is None:
        print("Could not auto-detect Steam's Quake III baseq3 directory.", file=sys.stderr)
        print("Re-run with --game-dir /path/to/baseq3", file=sys.stderr)
        return 2
    if not target.is_dir() or not (target / "pak0.pk3").is_file():
        print(f"Not a Quake III baseq3 directory with pak0.pk3: {target}", file=sys.stderr)
        return 2

    try:
        manifest = remote_manifest(args.remote, args.identity, args.port)
        print(f"Target: {target}")
        print(f"Server: {args.remote}; {len(manifest)} community PK3 files (stock pak*.pk3 excluded)")
        for name, expected in manifest.items():
            result = install_one(name, expected, target, args.remote, args.identity, args.port, args.dry_run)
            print(f"{name}: {result}")
    except subprocess.CalledProcessError as exc:
        print(exc.stderr.strip() or str(exc), file=sys.stderr)
        return exc.returncode or 1
    except RuntimeError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1

    print("Done. Restart Quake III if it was open so it rescans baseq3.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
