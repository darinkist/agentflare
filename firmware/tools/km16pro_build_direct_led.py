#!/usr/bin/env python3
"""Build the persistent direct-LED patch from a KM16 Pro application image.

Build mode writes the patch. Check mode verifies an existing backup/patch
pair before the manual flash step and prints the flash command. Check mode
never touches USB.

Safety comes from validating the exact bytes being replaced (hook sites),
the jump targets, and the erased handler area — not from matching one
whole-file hash, which no single value can cover for every stock revision.
"""

from __future__ import annotations

import argparse
import hashlib
import shutil
import struct
import subprocess
import sys
from pathlib import Path


APP_BASE = 0x08002000
APP_SIZE = 0xE000
HANDLER_ADDRESS = 0x0800FDC0
VIA_FALLBACK_HOOK = 0x0800AA20
PWM_FLUSH_WRAPPER = 0x080085B8
RGB_SET_COLOR_WRAPPER = 0x080085C4
RGB_SET_ALL_WRAPPER = 0x080085D4
R_ARM_THM_CALL = 10
R_ARM_THM_JUMP24 = 30
R_ARM_ABS32 = 2
SHN_ABS = 0xFFF1
MAX_HANDLER_SIZE = APP_BASE + APP_SIZE - HANDLER_ADDRESS
EXPECTED_BACKUP_SIZE = 122880
# Tested reference image (author's revision), informational only: the build
# gate below validates the exact bytes being replaced, not this hash. Other
# stock revisions with identical hook sites build fine too.
EXPECTED_BACKUP_SHA256 = "73b88b069a06c29b86d729fb7f1df1b36d3d36e4e72d4b0f8af2ba94e38540b8"
EXPECTED_PATCH_SIZE = 57344
EXPECTED_PATCH_SHA256 = "eda935bca0a1a743196011f8c814a15bf66c1cfc9bc3d0825bad9fe9606b559c"
# Original bytes at the four sites the patch overwrites (file offsets), read
# from the reference image. The patch replaces exactly these instructions with
# branches into the handler; anything else there means an unknown layout.
EXPECTED_HOOK_SITES = {
    VIA_FALLBACK_HOOK - APP_BASE: bytes.fromhex("fff704ff"),
    PWM_FLUSH_WRAPPER - APP_BASE: bytes.fromhex("014bdb68"),
    RGB_SET_COLOR_WRAPPER - APP_BASE: bytes.fromhex("10b4024c"),
    RGB_SET_ALL_WRAPPER - APP_BASE: bytes.fromhex("014b9b68"),
}
# First bytes at the three impl entry points the handler jumps to (file
# offsets; thumb bit cleared). Anchors that the jump targets start with the
# tested code.
EXPECTED_IMPL_PREFIXES = {
    (0x080091D5 & ~1) - APP_BASE: bytes.fromhex("f0b50c4d"),
    (0x08009211 & ~1) - APP_BASE: bytes.fromhex("70b51346"),
    (0x0800922D & ~1) - APP_BASE: bytes.fromhex("08b51b21"),
}
REPO_ROOT = Path(__file__).resolve().parents[2]


def u16(data: bytes, offset: int) -> int:
    return struct.unpack_from("<H", data, offset)[0]


def u32(data: bytes, offset: int) -> int:
    return struct.unpack_from("<I", data, offset)[0]


def encode_thumb_branch(source: int, target: int, *, link: bool) -> bytes:
    target &= ~1  # Function symbols carry the Thumb-state bit; branch offsets do not.
    offset = target - (source + 4)
    if offset & 1 or not -(1 << 24) <= offset < (1 << 24):
        raise ValueError(f"Thumb branch out of range: {source:#x} -> {target:#x}")
    encoded = offset & 0x01FFFFFF
    sign = (encoded >> 24) & 1
    i1 = (encoded >> 23) & 1
    i2 = (encoded >> 22) & 1
    imm10 = (encoded >> 12) & 0x3FF
    imm11 = (encoded >> 1) & 0x7FF
    j1 = 1 ^ (i1 ^ sign)
    j2 = 1 ^ (i2 ^ sign)
    first = 0xF000 | (sign << 10) | imm10
    second = (0xF800 if link else 0x9000) | (j1 << 13) | (j2 << 11) | imm11
    return struct.pack("<HH", first, second)


def apply_abs32_relocation(text: bytearray, offset: int, target: int) -> None:
    """Resolve an absolute literal relocation while retaining its addend."""
    addend = u32(text, offset)
    struct.pack_into("<I", text, offset, target + addend)


def validate_handler_size(size: int) -> None:
    if size > MAX_HANDLER_SIZE:
        raise ValueError(f"direct-LED handler is too large: {size} bytes")


def read_elf_text_and_symbols(path: Path) -> tuple[bytearray, dict[str, int], list[tuple[int, str, int]]]:
    data = path.read_bytes()
    if data[:4] != b"\x7fELF" or data[4] != 1 or data[5] != 1:
        raise ValueError("expected a little-endian ELF32 object")
    section_offset = u32(data, 32)
    section_size = u16(data, 46)
    section_count = u16(data, 48)
    section_names_index = u16(data, 50)
    sections = []
    for index in range(section_count):
        offset = section_offset + index * section_size
        sections.append({
            "name": u32(data, offset), "type": u32(data, offset + 4),
            "offset": u32(data, offset + 16), "size": u32(data, offset + 20),
            "link": u32(data, offset + 24), "info": u32(data, offset + 28),
            "entry_size": u32(data, offset + 36),
        })
    names = sections[section_names_index]
    names_data = data[names["offset"]:names["offset"] + names["size"]]

    def string(blob: bytes, offset: int) -> str:
        return blob[offset:blob.index(b"\0", offset)].decode()

    for section in sections:
        section["label"] = string(names_data, section["name"])
    text_index = next(i for i, section in enumerate(sections) if section["label"] == ".text")
    text_section = sections[text_index]
    text = bytearray(data[text_section["offset"]:text_section["offset"] + text_section["size"]])
    symtab = next(section for section in sections if section["type"] == 2)
    strtab = sections[symtab["link"]]
    strings = data[strtab["offset"]:strtab["offset"] + strtab["size"]]
    symbols: list[tuple[str, int, int]] = []
    exported: dict[str, int] = {}
    for offset in range(symtab["offset"], symtab["offset"] + symtab["size"], symtab["entry_size"]):
        name = string(strings, u32(data, offset))
        value = u32(data, offset + 4)
        section = u16(data, offset + 14)
        symbols.append((name, value, section))
        if name:
            exported[name] = value
    relocations: list[tuple[int, str, int]] = []
    for section in sections:
        if section["type"] != 9 or section["info"] != text_index:
            continue
        for offset in range(section["offset"], section["offset"] + section["size"], section["entry_size"]):
            reloc_offset = u32(data, offset)
            info = u32(data, offset + 4)
            symbol_index, reloc_type = info >> 8, info & 0xFF
            name, value, symbol_section = symbols[symbol_index]
            relocations.append((reloc_offset, name, reloc_type))
            if symbol_section == SHN_ABS:
                target = value
            elif symbol_section == text_index:
                target = HANDLER_ADDRESS + value
            else:
                raise ValueError(f"unsupported relocation target for {name}")
            if reloc_type == R_ARM_THM_CALL:
                text[reloc_offset:reloc_offset + 4] = encode_thumb_branch(
                    HANDLER_ADDRESS + reloc_offset, target, link=True
                )
            elif reloc_type == R_ARM_THM_JUMP24:
                text[reloc_offset:reloc_offset + 4] = encode_thumb_branch(
                    HANDLER_ADDRESS + reloc_offset, target, link=False
                )
            elif reloc_type == R_ARM_ABS32:
                # Literal-pool entries referring to local .text labels carry
                # their section-relative addend in the object file.
                apply_abs32_relocation(text, reloc_offset, target)
            else:
                raise ValueError(f"unsupported relocation {reloc_type} for {name}")
    return text, exported, relocations


def layout_problems(data: bytes) -> list[str]:
    """Check the exact bytes the patch overwrites and jumps to.

    This is the build safety gate, and deliberately not a whole-file hash:
    no single hash can cover every stock revision, but patching is safe
    whenever the replaced instructions and the jump targets match the tested
    layout. Returns a list of problems; empty means buildable.
    """
    problems = []
    for offset, expected in EXPECTED_HOOK_SITES.items():
        actual = data[offset:offset + len(expected)] if len(data) >= offset + len(expected) else b""
        if actual != expected:
            problems.append(f"hook site {APP_BASE + offset:#010x}: expected {expected.hex()}, found {actual.hex() or 'missing'}")
    for offset, expected in EXPECTED_IMPL_PREFIXES.items():
        actual = data[offset:offset + len(expected)] if len(data) >= offset + len(expected) else b""
        if actual != expected:
            problems.append(f"jump target {APP_BASE + offset:#010x}: expected {expected.hex()}, found {actual.hex() or 'missing'}")
    return problems


def handler_area_erased(image: bytearray, size: int) -> bool:
    """Check the handler region holds only erased (0xFF) bytes."""
    offset = HANDLER_ADDRESS - APP_BASE
    return all(byte == 0xFF for byte in image[offset:offset + size])


def validated_source(path: Path) -> bytes:
    if not path.is_file():
        raise ValueError(f"source image does not exist: {path}")
    source = path.read_bytes()
    if len(source) != EXPECTED_BACKUP_SIZE:
        raise ValueError(f"unsupported source image size: {len(source)} bytes")
    if problems := layout_problems(source):
        raise ValueError("source layout not recognized: " + "; ".join(problems))
    return source


def resolve_data_path(path: Path, *allowed_dirs: str) -> Path:
    """Allow given firmware data dirs or any location outside the repository.

    Other in-repo locations are refused so blobs stay where .gitignore covers
    them and cannot overwrite tracked sources.
    """
    resolved = path.expanduser().resolve()
    try:
        rel = resolved.relative_to(REPO_ROOT)
    except ValueError:
        return resolved
    if len(rel.parts) >= 2 and rel.parts[:2] in [("firmware", d) for d in allowed_dirs]:
        return resolved
    raise ValueError(
        f"refusing to use a file inside the repository outside "
        f"{'/'.join(['firmware/' + d for d in allowed_dirs])}: {resolved}"
    )


def check_pair(backup: Path, patch: Path) -> None:
    """Validate a backup/patch pair before the manual flash step."""
    backup_resolved = resolve_data_path(backup, "backups-local")
    patch_resolved = resolve_data_path(patch, "builds-local")
    if not backup_resolved.is_file():
        raise ValueError(f"backup does not exist: {backup_resolved}")
    if not patch_resolved.is_file():
        raise ValueError(f"patch does not exist: {patch_resolved}")
    backup_bytes = backup_resolved.read_bytes()
    if len(backup_bytes) != EXPECTED_BACKUP_SIZE:
        raise ValueError(f"unsupported backup size: {len(backup_bytes)} bytes")
    if problems := layout_problems(backup_bytes):
        raise ValueError("backup layout not recognized: " + "; ".join(problems))
    patch_bytes = patch_resolved.read_bytes()
    if len(patch_bytes) != EXPECTED_PATCH_SIZE:
        raise ValueError(f"unsupported patch size: {len(patch_bytes)} bytes")
    if hashlib.sha256(patch_bytes).hexdigest() != EXPECTED_PATCH_SHA256:
        raise ValueError("unsupported patch hash")
    if backup_bytes == patch_bytes:
        raise ValueError("backup and patch must be different files")
    if shutil.which("dfu-util") is None:
        raise ValueError("dfu-util is not installed")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path)
    parser.add_argument(
        "--assembly",
        type=Path,
        default=Path(__file__).parents[1] / "patch" / "direct-led" / "km16pro_direct_led.s",
    )
    parser.add_argument("--output", type=Path)
    parser.add_argument("--check", action="store_true", help="validate a backup/patch pair; never flashes")
    parser.add_argument("--backup", type=Path, help="stock backup for --check")
    parser.add_argument("--patch", type=Path, help="built patch for --check")
    args = parser.parse_args()

    if args.check:
        if args.backup is None or args.patch is None:
            parser.error("--check requires --backup and --patch")
        try:
            check_pair(args.backup, args.patch)
        except ValueError as exc:
            print(f"Error: {exc}", file=sys.stderr)
            raise SystemExit(2)
        print(f"backup OK: {EXPECTED_BACKUP_SIZE} bytes, hook sites and jump targets match the tested layout")
        print(f"patch OK: {EXPECTED_PATCH_SIZE} bytes, SHA-256 {EXPECTED_PATCH_SHA256}")
        print("Next: re-enter bootloader mode, take fresh values from `dfu-util -l`, then run:")
        print(f'dfu-util -d "$DFU_SELECTOR" -a "$DFU_APP_ALT" -D "{args.patch}"')
        return

    if args.source is None or args.output is None:
        parser.error("build mode requires --source and --output")

    if not args.assembly.is_file():
        raise ValueError(f"assembly source does not exist: {args.assembly}")
    source = validated_source(args.source)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    object_file = args.output.with_suffix(".o")
    subprocess.run([
        "clang", "-target", "armv7m-none-eabi", "-mthumb", "-c", str(args.assembly), "-o", str(object_file)
    ], check=True)
    text, symbols, relocations = read_elf_text_and_symbols(object_file)
    required_symbols = {"direct_handler", "direct_flush", "direct_guard_color", "direct_guard_all", "direct_rgb_set_call"}
    if symbols.get("direct_handler", -1) & ~1 or not required_symbols <= symbols.keys():
        raise ValueError("unexpected direct-LED symbol layout")
    rgb_call = HANDLER_ADDRESS + (symbols["direct_rgb_set_call"] & ~1)
    text[symbols["direct_rgb_set_call"] & ~1:(symbols["direct_rgb_set_call"] & ~1) + 4] = encode_thumb_branch(
        rgb_call, 0x080091d5, link=True
    )
    validate_handler_size(len(text))
    image = bytearray(source[:APP_SIZE])
    handler_offset = HANDLER_ADDRESS - APP_BASE
    if not handler_area_erased(image, len(text)):
        raise ValueError("direct-LED handler area is not erased in the source dump")
    image[VIA_FALLBACK_HOOK - APP_BASE:VIA_FALLBACK_HOOK - APP_BASE + 4] = encode_thumb_branch(
        VIA_FALLBACK_HOOK, HANDLER_ADDRESS, link=True
    )
    image[PWM_FLUSH_WRAPPER - APP_BASE:PWM_FLUSH_WRAPPER - APP_BASE + 4] = encode_thumb_branch(
        PWM_FLUSH_WRAPPER, HANDLER_ADDRESS + (symbols["direct_flush"] & ~1), link=False
    )
    image[RGB_SET_COLOR_WRAPPER - APP_BASE:RGB_SET_COLOR_WRAPPER - APP_BASE + 4] = encode_thumb_branch(
        RGB_SET_COLOR_WRAPPER, HANDLER_ADDRESS + (symbols["direct_guard_color"] & ~1), link=False
    )
    image[RGB_SET_ALL_WRAPPER - APP_BASE:RGB_SET_ALL_WRAPPER - APP_BASE + 4] = encode_thumb_branch(
        RGB_SET_ALL_WRAPPER, HANDLER_ADDRESS + (symbols["direct_guard_all"] & ~1), link=False
    )
    image[handler_offset:handler_offset + len(text)] = text
    args.output.write_bytes(image)
    print(f"direct-LED handler: {len(text)} bytes at {HANDLER_ADDRESS:#010x}")
    print(f"direct flush: {HANDLER_ADDRESS + (symbols['direct_flush'] & ~1):#010x}")
    print(f"output: {args.output}")
    print(f"output size: {len(image)} bytes")
    print(f"output SHA-256: {hashlib.sha256(image).hexdigest()}")


if __name__ == "__main__":
    main()
