#!/usr/bin/env python3
"""Install q3ctl's community-map catalog from canonical LvL World sources.

This script does not connect to the game server.  It fetches the five original
LvL World archives which contain the exact non-stock PK3 files installed by the
q3ctl server, verifies both the published archive checksum and every extracted
PK3 checksum, then installs the PK3s into a local Quake III baseq3 directory.

Usage:
  python3 tools/sync_server_maps.py --dry-run
  python3 tools/sync_server_maps.py
  python3 tools/sync_server_maps.py --game-dir /path/to/Quake\ III\ Arena/baseq3

Steam's macOS installation location is discovered automatically when possible.
Existing divergent files are moved to q3ctl-map-backups/<timestamp>/; stock
pak*.pk3 files are never touched.
"""

from __future__ import annotations

import argparse
import hashlib
import os
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path, PurePosixPath
import re
import shutil
import sys
import tempfile
from urllib.error import URLError
import urllib.request
import zipfile

LVL_WORLD = "https://lvlworld.com"
USER_AGENT = "q3ctl-canonical-map-sync/1.0"
TOKEN_RE = re.compile(
    r'location\s*=\s*"/dl/"\s*\+\s*s\s*\+\s*"/(?P<id>\d+)/'
    r'(?P<first>[0-9a-f]+)/(?P<second>[0-9a-f]+)"'
)


@dataclass(frozen=True)
class SourcePack:
    """One original archive and the exact PK3 files q3ctl uses from it."""

    levelworld_id: int
    archive_name: str
    archive_sha256: str
    pk3_sha256: dict[str, str]


# Archive checksums are the SHA-256 values published on the LvL World download
# pages.  PK3 checksums are the verified contents currently installed on the
# server, so a successful run gives the client byte-identical custom content.
PACKS: tuple[SourcePack, ...] = (
    SourcePack(
        2577,
        "ospmaps0.zip",
        "3627f8378a40a485cefe6b0d2a7e4dd24292ff2703a73302558fdcba3644821b",
        {"ospmaps0.pk3": "b5dacaf7f1203c43e5baabb8b3824b00330e7d5a6c620626f883300b67c9b748"},
    ),
    SourcePack(
        2070,
        "lvl_10th_anniversary_ctf.zip",
        "7bbd6cf8dbdcfb0f0b88c3db521ca9b7cad4c17821a33aa392c078d883aeaa1f",
        {
            "bastir.pk3": "322c35ea6fbcc4cbb412a74f5cc27caa937f06658f36f2c63f0227ee379c1a19",
            "bubctf1.pk3": "7dafd7754b844f3db620b51eecb057eb2463ceaa21928df020f9b05705346b24",
            "ctctf.pk3": "05296934df47e00c259060034141420c24c65457470ac691d204fe2444d67492",
            "frozencolors.pk3": "d50b71460e208e3fa28575dece8b88f294a4bde09c24b74a9d971a1d364c1b2a",
            "geit3ctf1.pk3": "df80256558d84b6c1453519761f09207840f0a79513b4465a303dae2d89044b5",
            "geit3ctf2.pk3": "b9ecced1643bae8656f99de8c295436aade7dd98a87fc28833814b1ebe423002",
            "halq3ctf7.pk3": "65914cc45c4c68550f5a08809d5e7db9c52aa433d2efc4d75236e51c4745318b",
            "jof3ctf1.pk3": "55545e49fa45e8303caab1532bf022d7884db7df92a42eda9181f2dbb6e1225a",
            "klzicecoldctf.pk3": "127fbb27b707a8c29602caf5ecb858820a1a1faade92b0d439090557c6137191",
            "map-kellblack.pk3": "d2277ef2f43e4b8fe393f3f886dd29cab35cbcd0a6e3c0c859744db4da6d0ccc",
            "mapel4b.pk3": "596962c03a7632692d7a535239688b52eee6b570d0b7b7e2e228aab24c4b803a",
            "mIKEctf3.pk3": "e816d9cff328b8788673864682290605c8ebc7f1331bd782c875c542fc98f058",
            "mkbase.pk3": "9cb74123255193176b24531126c5e66fa53976e59b983f6e7b8bf6abe86f6492",
            "on-xctf5.pk3": "e09684f670b351250fb68d51fbf267947e11d40d899e0d2e219453fd490dcb20",
            "rota3ctf1.pk3": "c2d35fd8c877b5d9366df2c4545598443f4f0e1472f4ee9a7024081c6708dcdb",
            "rota3ctf2.pk3": "bf6a6a7f7634cb8f054914db5cd75b67dcd512ee0f4ff58e9ff195d39723b9eb",
        },
    ),
    SourcePack(
        2069,
        "lvl_10th_anniversary_tdm.zip",
        "e0e2facc270da2e2e0848e947653c1baa2345c1a2318d5f79146061b5437bd29",
        {
            "anodm4.pk3": "64f4c031681560b9a15ab3c7e02f88026cbe487a4c723d7542a0db273844b8c3",
            "batcula.pk3": "fde14206d1a79fd62f4f252023ae678a288c988669f39bc106cfe86ad7b52872",
            "ci.pk3": "03b36293af5900fe2368f852b3b984abc59a87f97ec92da8519e08f6cdca6b77",
            "ktsdm3v2.pk3": "bd35a05fd2cda3c61325f0f905e0e169f958f3a68fa1c75f338dc31601a64547",
            "map_nodm4.pk3": "a59b07eb6114fcf25c1c78e0b4eceb8934ec8d46443a99ffb1a5fe193e6aab4f",
            "map-qfraggel2a.pk3": "7caab526e947187ebe43fa0730960d1b64adb56626733fc722c6d86ece3fda24",
            "map-qfraggel3.pk3": "55bd1dbf32dd0c17018582968f16283ed6946742a36da80305269281f995592f",
            "mkexp.pk3": "26f35482d5204efcc046322c5db560fb0aa89f761f92d49939f497c9d63acb1b",
            "pro-dcmap7.pk3": "12b249b5eba82a2f30a4e2c55876fa662dc1325d9b28f5524507abb2a25dd419",
            "pukka3dm1.pk3": "d1c6c629f2f779e74eeb394c060bf16b5d5cd1d4c83eb29594281891896a9d5d",
            "teddm2.pk3": "0604f230b93e06ea14a29e40042a7ded4b9898a86eec20874798051ff8499ad2",
            "ts_dm5tmp.pk3": "259e888b831c086396c7ba5213d7ba1110dd22ca1cebd83d4083d51bf80b072e",
        },
    ),
    SourcePack(
        2271,
        "alliance_maps_1.zip",
        "18d5c3faede5023524d3a67c7808b5a94ac7035bea37113a8af8c1d1a7779438",
        {
            "alliance_maps01.pk3": "5f27ac726793d7a3b54cb2f9b8383a9eceb665ead844bf48999705db0166baac",
            "alliance_maps02.pk3": "141c4e7a1a30cc495f1ba186092b6ba59eba1a4a8742182dcebaea711cbf416d",
            "alliance_maps03.pk3": "1e5e27af1065e7d4d38d54ca7b39a2d83abd0d1c0b11c4b8ec1ba6b6516e5b0a",
            "alliance_maps04.pk3": "aec61afd01775a8192c6ad7ac4276656e04c0c603a6ee09d52882ee0cca65894",
        },
    ),
    SourcePack(
        2270,
        "alliance_maps_2.zip",
        "2a040e9613e5ebf89c3fbaa2820b37d2c485b31f19e8fd35ae72d6244c76c161",
        {"alliance_maps05.pk3": "1f8e618d5df6fb4e958564d8f8e735bc25c8512ebf0d4ba39b0a0a8ed9a43f4f"},
    ),
)


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def default_game_dirs() -> list[Path]:
    home = Path.home()
    return [
        home / "Library/Application Support/Steam/steamapps/common/Quake III Arena/baseq3",
        home / "Library/Application Support/Steam/steamapps/common/Quake 3 Arena/baseq3",
        home / ".steam/steam/steamapps/common/Quake III Arena/baseq3",
        home / ".local/share/Steam/steamapps/common/Quake III Arena/baseq3",
    ]


def resolve_game_dir(requested: str | None) -> Path:
    if requested:
        return Path(requested).expanduser().resolve()
    for candidate in default_game_dirs():
        if candidate.is_dir():
            return candidate.resolve()
    choices = "\n  ".join(str(path) for path in default_game_dirs())
    raise RuntimeError("Could not find Steam's baseq3 directory. Use --game-dir. Tried:\n  " + choices)


def resolve_download(pack: SourcePack) -> str:
    page_url = f"{LVL_WORLD}/download/id:{pack.levelworld_id}"
    request = urllib.request.Request(page_url, headers={"User-Agent": USER_AGENT})
    with urllib.request.urlopen(request, timeout=45) as response:
        page = response.read().decode("utf-8", "replace")
    match = TOKEN_RE.search(page)
    if not match or int(match.group("id")) != pack.levelworld_id:
        raise RuntimeError(f"LvL World did not provide a download token for {pack.archive_name} ({page_url})")
    return (
        f"{LVL_WORLD}/dl/lvl/{pack.levelworld_id}/"
        f"{match.group('first')}/{match.group('second')}"
    )


def download_verified(pack: SourcePack, destination: Path) -> None:
    url = resolve_download(pack)
    print(f"Downloading {pack.archive_name} from LvL World …")
    request = urllib.request.Request(url, headers={"User-Agent": USER_AGENT})
    with urllib.request.urlopen(request, timeout=180) as response, destination.open("wb") as output:
        shutil.copyfileobj(response, output, length=1024 * 1024)
    actual = sha256(destination)
    if actual != pack.archive_sha256:
        destination.unlink(missing_ok=True)
        raise RuntimeError(
            f"Archive checksum mismatch for {pack.archive_name}: expected "
            f"{pack.archive_sha256}, got {actual}"
        )


def verify_and_extract(pack: SourcePack, archive: Path, destination: Path) -> dict[str, Path]:
    extracted: dict[str, Path] = {}
    with zipfile.ZipFile(archive) as bundle:
        for info in bundle.infolist():
            member = PurePosixPath(info.filename)
            if member.is_absolute() or ".." in member.parts:
                raise RuntimeError(f"Unsafe archive path in {pack.archive_name}: {info.filename!r}")
            name = member.name
            if name not in pack.pk3_sha256:
                continue
            if member.suffix.lower() != ".pk3" or not name:
                raise RuntimeError(f"Unexpected PK3 member in {pack.archive_name}: {info.filename!r}")
            target = destination / name
            with bundle.open(info) as source, target.open("wb") as output:
                shutil.copyfileobj(source, output, length=1024 * 1024)
            actual = sha256(target)
            expected = pack.pk3_sha256[name]
            if actual != expected:
                raise RuntimeError(f"PK3 checksum mismatch for {name}: expected {expected}, got {actual}")
            extracted[name] = target
    expected_names = set(pack.pk3_sha256)
    if set(extracted) != expected_names:
        missing = sorted(expected_names - set(extracted))
        unexpected = sorted(set(extracted) - expected_names)
        raise RuntimeError(f"Archive content mismatch for {pack.archive_name}; missing={missing}, unexpected={unexpected}")
    return extracted


def install_one(source: Path, target: Path, expected_sha256: str, backup_root: Path, dry_run: bool) -> str:
    if target.name.lower().startswith("pak") and target.name.lower().endswith(".pk3"):
        raise RuntimeError(f"Refusing to alter stock archive name: {target.name}")
    if target.exists() and sha256(target) == expected_sha256:
        return "already present"
    if dry_run:
        return "would install" if not target.exists() else "would back up and replace"
    if target.exists():
        backup_root.mkdir(parents=True, exist_ok=True)
        backup = backup_root / target.name
        shutil.move(str(target), str(backup))
    temporary = target.with_suffix(target.suffix + ".q3ctl-new")
    shutil.copy2(source, temporary)
    os.replace(temporary, target)
    if sha256(target) != expected_sha256:
        raise RuntimeError(f"Post-install checksum mismatch for {target}")
    return "installed"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--game-dir", help="Steam Quake III baseq3 directory (auto-detected by default)")
    parser.add_argument("--dry-run", action="store_true", help="resolve canonical sources and show planned changes")
    args = parser.parse_args()

    target_dir = resolve_game_dir(args.game_dir)
    if not args.dry_run and not target_dir.is_dir():
        raise RuntimeError(f"Game directory does not exist: {target_dir}")

    all_maps = {name: digest for pack in PACKS for name, digest in pack.pk3_sha256.items()}
    print(f"Target: {target_dir}")
    print(f"Canonical source archives: {len(PACKS)}; exact custom PK3s: {len(all_maps)}")
    if args.dry_run:
        for pack in PACKS:
            url = resolve_download(pack)
            print(f"  {pack.archive_name}: {len(pack.pk3_sha256)} PK3s; source resolved")
            print(f"    {url}")
        for name, digest in sorted(all_maps.items(), key=lambda item: item[0].lower()):
            state = "already present" if (target_dir / name).is_file() and sha256(target_dir / name) == digest else "would install"
            print(f"  {name}: {state}")
        return 0

    backup_root = target_dir / "q3ctl-map-backups" / datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    with tempfile.TemporaryDirectory(prefix="q3ctl-canonical-maps-") as temporary_dir:
        temporary = Path(temporary_dir)
        staged: dict[str, Path] = {}
        for pack in PACKS:
            archive = temporary / f"{pack.levelworld_id}.zip"
            download_verified(pack, archive)
            pack_dir = temporary / str(pack.levelworld_id)
            pack_dir.mkdir()
            staged.update(verify_and_extract(pack, archive, pack_dir))
        if set(staged) != set(all_maps):
            raise RuntimeError("Internal error: staged map set does not match manifest")
        for name in sorted(staged, key=str.lower):
            action = install_one(staged[name], target_dir / name, all_maps[name], backup_root, False)
            print(f"{name}: {action}")
    print("Completed. Restart Quake III before joining the server.")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, RuntimeError, URLError, zipfile.BadZipFile) as error:
        print(f"error: {error}", file=sys.stderr)
        raise SystemExit(1)
