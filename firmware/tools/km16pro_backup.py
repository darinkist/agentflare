#!/usr/bin/env python3
"""Backup the KM16 Pro application image over DFU.

Reads the application area twice and verifies both reads are byte-identical.
The backup always succeeds when the two reads match, whatever is on the
device. The verdict compares against the tested reference image and the hook
sites the patch replaces: byte-identical means VERIFIED STOCK; matching hook
sites with differing settings bytes (normal after boot, or another stock
revision) means PATCH-COMPATIBLE LAYOUT and is expected to build; anything
else is kept as a DEVICE SNAPSHOT. No single hash is treated as universal
truth for every stock revision.

This script never erases or flashes; it only reads.

Example (dry-run first, no USB access; take both values from a fresh
`dfu-util -l` on the device you intend to back up):
  python km16pro_backup.py --selector '<VID:PID>' --alt '<ALT>' --name 'my-pad'
  python km16pro_backup.py --execute --selector '<VID:PID>' --alt '<ALT>' --name 'my-pad'

If you just flashed a known file, add `--reference <that file>`: the script
then reports whether the reads match it and, if not, how much differs and
whether the code region is intact. (The device rewrites a few settings bytes
in flash on boot, so a readback is expected to differ slightly from the file
that was written.)

Copy --selector (<VID:PID>) and --alt (<ALT>) from a fresh `dfu-util -l` output on the
device you intend to back up. Never reuse a previously recorded USB path.
Outputs default to firmware/backups-local/ in the repository working tree.
That directory is gitignored, so blobs are never committed.
"""

from __future__ import annotations

import argparse
import hashlib
import re
import subprocess
import sys
from pathlib import Path


EXPECTED_SIZE = 122880
# Tested reference image (author's revision), informational only. Safety comes
# from the hook-site check below, not from matching this hash: other stock
# revisions with identical hook sites back up and build fine too.
EXPECTED_SHA256 = "73b88b069a06c29b86d729fb7f1df1b36d3d36e4e72d4b0f8af2ba94e38540b8"
PATCH_REGION_SIZE = 57344
# Original bytes at the four sites the patch overwrites (offsets into the
# 122880-byte application image). Duplicated from km16pro_build_direct_led.py
# so this script stays standalone; the builder re-verifies them anyway.
EXPECTED_HOOK_SITES = {
    0x8A20: bytes.fromhex("fff704ff"),
    0x65B8: bytes.fromhex("014bdb68"),
    0x65C4: bytes.fromhex("10b4024c"),
    0x65D4: bytes.fromhex("014b9b68"),
}
REPO_ROOT = Path(__file__).resolve().parents[2]
NAME_PATTERN = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]*")
SELECTOR_PATTERN = re.compile(r"[0-9a-fA-F]{4}:[0-9a-fA-F]{4}")
ALT_PATTERN = re.compile(r"[0-9]+")
SMART_QUOTES = "\u2018\u2019\u201a\u201c\u201d\u201e\u00ab\u00bb"


def clean_arg(value: str) -> str:
    """Strip whitespace and macOS smart quotes pasted from rich text."""
    return value.strip().strip(SMART_QUOTES).strip()


def validated_selector(value: str) -> str:
    cleaned = clean_arg(value)
    if not SELECTOR_PATTERN.fullmatch(cleaned):
        raise ValueError(
            f"invalid selector {value!r}: expected VID:PID in hex, e.g. 1eaf:0003 "
            f"(use straight quotes or none at all)"
        )
    return cleaned.lower()


def validated_alt(value: str) -> str:
    cleaned = clean_arg(value)
    if not ALT_PATTERN.fullmatch(cleaned):
        raise ValueError(
            f"invalid alt {value!r}: expected digits, e.g. 2 (use straight quotes or none at all)"
        )
    return cleaned


def default_output(name: str) -> Path:
    return REPO_ROOT / "firmware" / "backups-local" / name


def validate_name(name: str) -> str:
    if not NAME_PATTERN.fullmatch(name):
        raise ValueError(
            f"invalid backup name {name!r}: use letters, digits, dot, underscore, or hyphen"
        )
    return name


def outputs_for_name(name: str) -> tuple[Path, Path]:
    stem = validate_name(name)
    return (default_output(f"{stem}-read-1.bin"), default_output(f"{stem}-read-2.bin"))


def ensure_writable(first: Path, second: Path) -> None:
    """Refuse to silently overwrite a previous backup; pass --overwrite to allow it."""
    existing = [str(p) for p in (first, second) if p.exists()]
    if existing:
        raise ValueError(
            f"output already exists (use --overwrite to replace): {', '.join(existing)}"
        )


def resolve_output_path(path: Path) -> Path:
    """Allow firmware/backups-local/ or any location outside the repository.

    Other in-repo locations are refused so blobs stay where .gitignore covers
    them and cannot overwrite tracked sources.
    """
    resolved = path.expanduser().resolve()
    try:
        rel = resolved.relative_to(REPO_ROOT)
    except ValueError:
        return resolved
    if len(rel.parts) >= 2 and rel.parts[:2] == ("firmware", "backups-local"):
        return resolved
    raise ValueError(f"refusing to write backup inside the repository outside firmware/backups-local/: {resolved}")


def sha256_of(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(65536), b""):
            digest.update(chunk)
    return digest.hexdigest()


def run_read(selector: str, alt: str, output: Path) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        ["dfu-util", "-d", selector, "-a", alt, "-U", str(output)],
        text=True,
        capture_output=True,
    )


def read_acceptable(result: subprocess.CompletedProcess[str], output: Path) -> bool:
    """Accept a completed read or the narrow full-upload LIBUSB pipe quirk.

    Any non-empty file counts: the size check against the known stock layout
    happens later as a reported verdict, not here.
    """
    if not output.is_file() or output.stat().st_size == 0:
        return False
    if result.returncode == 0:
        return True
    combined = (result.stdout + result.stderr).lower()
    return "libusb_error_pipe" in combined or "error during upload" in combined


def hook_sites_match(data: bytes) -> bool:
    """Check the four instructions the patch replaces.

    Same table as the builder's gate: patching is safe whenever these match,
    regardless of the overall file hash.
    """
    return all(
        len(data) >= offset + len(expected) and data[offset:offset + len(expected)] == expected
        for offset, expected in EXPECTED_HOOK_SITES.items()
    )


def verdict_for(size: int, digest: str, sites_ok: bool) -> str:
    """Classify matching reads.

    VERIFIED STOCK: byte-identical to the tested reference image.
    PATCH-COMPATIBLE LAYOUT: hook sites match (e.g. boot-rewritten settings
    bytes elsewhere, or another stock revision) — expected to build; the step 5
    check verifies fully before any flash.
    DEVICE SNAPSHOT: hook sites differ (custom firmware) — keep, don't build.
    """
    if size == EXPECTED_SIZE and digest == EXPECTED_SHA256:
        return "VERIFIED STOCK"
    if size == EXPECTED_SIZE and sites_ok:
        return "PATCH-COMPATIBLE LAYOUT"
    return "DEVICE SNAPSHOT (unrecognized layout)"


def compare_to_reference(reads: bytes, ref: bytes, ref_name: str) -> str:
    """Compare matching reads against a known file (e.g. the image just flashed).

    The device rewrites a few settings bytes in flash on boot, so a readback of
    a freshly flashed device is expected to differ slightly from the file that
    was written. This reports how much differs and whether the code region
    (first PATCH_REGION_SIZE bytes) is intact.
    """
    if reads == ref:
        return f"IDENTICAL TO REFERENCE ({ref_name})"
    if len(reads) != len(ref):
        return (
            f"DIFFERS FROM REFERENCE ({ref_name}): reads are {len(reads)} bytes, "
            f"reference is {len(ref)} bytes"
        )
    differing = sum(1 for a, b in zip(reads, ref) if a != b)
    region = "intact" if reads[:PATCH_REGION_SIZE] == ref[:PATCH_REGION_SIZE] else "DIFFERS"
    detail = (
        f"{differing} of {len(reads)} bytes differ, patch region {region}"
    )
    if region == "intact":
        return (
            f"DIFFERS FROM REFERENCE ({ref_name}): {detail} — stock code, "
            f"device settings changed (normal after boot)"
        )
    return f"DIFFERS FROM REFERENCE ({ref_name}): {detail}"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--selector", required=True, help="<VID:PID> selector from fresh `dfu-util -l` output")
    parser.add_argument("--alt", required=True, help="<ALT> value whose name contains 0x08002000 (shown as 0x8002000)")
    parser.add_argument("--first", type=Path, default=None, help="explicit first read output (use with --second instead of --name)")
    parser.add_argument("--second", type=Path, default=None, help="explicit second read output (use with --first instead of --name)")
    parser.add_argument("--name", default=None, help="backup name stem, e.g. 'my-pad' writes firmware/backups-local/my-pad-read-1.bin and my-pad-read-2.bin")
    parser.add_argument("--reference", type=Path, default=None, help="known file to compare the reads against, e.g. the image just flashed")
    parser.add_argument("--overwrite", action="store_true", help="allow overwriting existing output files")
    parser.add_argument("--execute", action="store_true", help="Actually run dfu-util. Without this, print only.")
    args = parser.parse_args()

    try:
        selector = validated_selector(args.selector)
        alt = validated_alt(args.alt)
    except ValueError as exc:
        parser.error(str(exc))

    if args.name is not None:
        if args.first is not None or args.second is not None:
            parser.error("--name cannot be combined with --first or --second")
        try:
            first_arg, second_arg = outputs_for_name(args.name)
        except ValueError as exc:
            parser.error(str(exc))
    elif args.first is not None and args.second is not None:
        first_arg, second_arg = args.first, args.second
    else:
        parser.error("give the backup a name with --name (e.g. --name 'my-pad'), or explicit paths with --first and --second")

    first = resolve_output_path(first_arg)
    second = resolve_output_path(second_arg)
    if first == second:
        parser.error("--first and --second must be different files")
    if not args.overwrite:
        try:
            ensure_writable(first, second)
        except ValueError as exc:
            parser.error(str(exc))

    commands = [
        ["dfu-util", "-d", selector, "-a", alt, "-U", str(first)],
        ["dfu-util", "-d", selector, "-a", alt, "-U", str(second)],
    ]
    if not args.execute:
        for command in commands:
            print("DRY-RUN", " ".join(command))
        print(f"Would verify both files are byte-identical and report size and SHA-256 (known stock: {EXPECTED_SIZE} bytes, {EXPECTED_SHA256}).")
        print("Add --execute to run. Files stay under firmware/backups-local/ and are never committed.")
        return 0

    for command, output in zip(commands, (first, second)):
        output.parent.mkdir(parents=True, exist_ok=True)
        result = run_read(selector, alt, output)
        if not read_acceptable(result, output):
            print(f"Error: read failed for {output}", file=sys.stderr)
            print(result.stdout, file=sys.stderr)
            print(result.stderr, file=sys.stderr)
            return 2
        print(f"read {output}: {output.stat().st_size} bytes")

    first_bytes = first.read_bytes()
    second_bytes = second.read_bytes()
    if first_bytes != second_bytes:
        print("Error: two reads differ; keep both files and stop", file=sys.stderr)
        return 2
    size = len(first_bytes)
    digest = hashlib.sha256(first_bytes).hexdigest()
    print("two reads match")
    print(f"size: {size} bytes")
    print(f"SHA-256: {digest}")
    verdict = verdict_for(size, digest, hook_sites_match(first_bytes))
    print(f"verdict: {verdict}")
    if verdict == "VERIFIED STOCK":
        print("This backup can be used to build the patch.")
    elif verdict == "PATCH-COMPATIBLE LAYOUT":
        print("Hook sites match the tested layout (only settings bytes differ).")
        print("This backup is expected to build; the step 5 check verifies fully.")
    else:
        print("Kept as a device snapshot. Building the patch requires an image whose")
        print("hook sites match the tested layout; never build from custom firmware.")
    if args.reference is not None:
        ref_path = args.reference.expanduser()
        if not ref_path.is_file():
            print(f"Error: reference does not exist: {ref_path}", file=sys.stderr)
            return 2
        print(compare_to_reference(first_bytes, ref_path.read_bytes(), str(args.reference)))
    print("Store one copy on separate physical or cloud-backed storage before any flash work.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
