#!/usr/bin/env python3

import importlib.util
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


PATH = Path(__file__).with_name("km16pro_build_direct_led.py")
SPEC = importlib.util.spec_from_file_location("km16pro_build_direct_led", PATH)
assert SPEC is not None and SPEC.loader is not None
build_direct_led = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(build_direct_led)

BACKUP_PATH = Path(__file__).with_name("km16pro_backup.py")
BACKUP_SPEC = importlib.util.spec_from_file_location("km16pro_backup", BACKUP_PATH)
assert BACKUP_SPEC is not None and BACKUP_SPEC.loader is not None
backup = importlib.util.module_from_spec(BACKUP_SPEC)
BACKUP_SPEC.loader.exec_module(backup)


class BuildDirectLedTest(unittest.TestCase):
    def test_thumb_branch_encoding_covers_forward_and_backward_calls(self):
        self.assertEqual(
            build_direct_led.encode_thumb_branch(0x08001000, 0x08002000, link=False).hex(),
            "00f0febf",
        )
        self.assertEqual(
            build_direct_led.encode_thumb_branch(0x08002000, 0x08001000, link=True).hex(),
            "fef7feff",
        )

    def test_absolute_relocation_retains_literal_addend(self):
        text = bytearray((0x34, 0x12, 0x00, 0x00))
        build_direct_led.apply_abs32_relocation(text, 0, 0x08000000)
        self.assertEqual(text, bytearray((0x34, 0x12, 0x00, 0x08)))

    def test_handler_layout_limit_is_rejected(self):
        with self.assertRaisesRegex(ValueError, "handler is too large"):
            build_direct_led.validate_handler_size(build_direct_led.MAX_HANDLER_SIZE + 1)

    def test_public_assembly_build_has_expected_symbols_and_relocations(self):
        if shutil.which("clang") is None:
            self.skipTest("clang is not installed")
        with tempfile.TemporaryDirectory() as directory:
            object_file = Path(directory) / "directled.o"
            subprocess.run(
                [
                    "clang",
                    "-target",
                    "armv7m-none-eabi",
                    "-mthumb",
                    "-c",
                    str(PATH.parent / "../patch/direct-led/km16pro_direct_led.s"),
                    "-o",
                    str(object_file),
                ],
                check=True,
            )
            text, symbols, relocations = build_direct_led.read_elf_text_and_symbols(object_file)
            self.assertLessEqual(len(text), build_direct_led.MAX_HANDLER_SIZE)
            self.assertTrue(
                {"direct_handler", "direct_flush", "direct_guard_color", "direct_guard_all", "direct_rgb_set_call"}
                <= symbols.keys()
            )
            self.assertEqual(relocations, [(34, "direct_handler", 10), (42, "direct_flush", 30)])

    def test_rejects_unsupported_size_before_building(self):
        with tempfile.TemporaryDirectory() as directory:
            image = Path(directory) / "short.bin"
            image.write_bytes(b"not a firmware image")
            with self.assertRaisesRegex(ValueError, "unsupported source image size"):
                build_direct_led.validated_source(image)

    def test_rejects_same_size_unknown_image(self):
        with tempfile.TemporaryDirectory() as directory:
            image = Path(directory) / "unknown.bin"
            image.write_bytes(bytes(122880))
            with self.assertRaisesRegex(ValueError, "layout not recognized"):
                build_direct_led.validated_source(image)

    def test_check_rejects_missing_files(self):
        with tempfile.TemporaryDirectory() as directory:
            missing = Path(directory) / "missing.bin"
            other = Path(directory) / "other.bin"
            other.write_bytes(b"x")
            with self.assertRaisesRegex(ValueError, "does not exist"):
                build_direct_led.check_pair(missing, other)

    def test_check_rejects_wrong_backup_size(self):
        with tempfile.TemporaryDirectory() as directory:
            backup = Path(directory) / "backup.bin"
            patch = Path(directory) / "patch.bin"
            backup.write_bytes(b"too short")
            patch.write_bytes(bytes(build_direct_led.EXPECTED_PATCH_SIZE))
            with self.assertRaisesRegex(ValueError, "unsupported backup size"):
                build_direct_led.check_pair(backup, patch)

    def test_check_rejects_unrecognized_backup_layout(self):
        with tempfile.TemporaryDirectory() as directory:
            backup = Path(directory) / "backup.bin"
            patch = Path(directory) / "patch.bin"
            backup.write_bytes(bytes(build_direct_led.EXPECTED_BACKUP_SIZE))
            patch.write_bytes(bytes(build_direct_led.EXPECTED_PATCH_SIZE))
            with self.assertRaisesRegex(ValueError, "layout not recognized"):
                build_direct_led.check_pair(backup, patch)

    def test_check_refuses_files_inside_repo(self):
        repo_file = build_direct_led.REPO_ROOT / "inside-repo-check.bin"
        with tempfile.TemporaryDirectory() as directory:
            other = Path(directory) / "other.bin"
            other.write_bytes(b"x")
            with self.assertRaisesRegex(ValueError, "inside the repository"):
                build_direct_led.check_pair(repo_file, other)

    def test_check_allows_local_data_dirs(self):
        allowed_backup = build_direct_led.REPO_ROOT / "firmware" / "backups-local" / "x.bin"
        self.assertEqual(
            build_direct_led.resolve_data_path(allowed_backup, "backups-local"), allowed_backup
        )
        with self.assertRaisesRegex(ValueError, "inside the repository"):
            build_direct_led.resolve_data_path(
                build_direct_led.REPO_ROOT / "firmware" / "patch" / "x.bin", "backups-local"
            )


class BackupNameTest(unittest.TestCase):
    def test_name_derives_two_read_paths(self):
        first, second = backup.outputs_for_name("my-pad")
        self.assertEqual(first.name, "my-pad-read-1.bin")
        self.assertEqual(second.name, "my-pad-read-2.bin")
        self.assertNotEqual(first, second)

    def test_name_defaults_into_backups_local(self):
        first, _ = backup.outputs_for_name("my-pad")
        self.assertEqual(
            first.parent,
            backup.REPO_ROOT / "firmware" / "backups-local",
        )

    def test_outputs_accept_backups_local_but_refuse_tracked_dirs(self):
        allowed = backup.REPO_ROOT / "firmware" / "backups-local" / "my-pad-read-1.bin"
        backup.resolve_output_path(allowed)
        with self.assertRaisesRegex(ValueError, "inside the repository"):
            backup.resolve_output_path(backup.REPO_ROOT / "firmware" / "patch" / "x.bin")

    def test_selector_accepts_hex_vid_pid(self):
        self.assertEqual(backup.validated_selector("1eaf:0003"), "1eaf:0003")
        self.assertEqual(backup.validated_selector("1EAF:0003"), "1eaf:0003")

    def test_selector_rejects_garbage(self):
        for bad in ("1eaf", "xyz:1234", "1eaf:0003:5", ""):
            with self.assertRaisesRegex(ValueError, "invalid selector"):
                backup.validated_selector(bad)

    def test_smart_quotes_are_stripped(self):
        self.assertEqual(backup.validated_selector("\u201e1eaf:0003\u201c"), "1eaf:0003")
        self.assertEqual(backup.validated_alt("\u201a2\u2018"), "2")

    def test_alt_rejects_non_digits(self):
        for bad in ("2a", "alt2", ""):
            with self.assertRaisesRegex(ValueError, "invalid alt"):
                backup.validated_alt(bad)

    def test_name_rejects_path_traversal_and_separators(self):
        for bad in ("../escape", "a/b", "", ".hidden", "-lead"):
            with self.assertRaisesRegex(ValueError, "invalid backup name"):
                backup.outputs_for_name(bad)

    def test_ensure_writable_passes_for_missing_files(self):
        with tempfile.TemporaryDirectory() as directory:
            backup.ensure_writable(Path(directory) / "a.bin", Path(directory) / "b.bin")

    def test_ensure_writable_refuses_existing_files(self):
        with tempfile.TemporaryDirectory() as directory:
            existing = Path(directory) / "a.bin"
            existing.write_bytes(b"x")
            with self.assertRaisesRegex(ValueError, "output already exists"):
                backup.ensure_writable(existing, Path(directory) / "b.bin")

    def test_verdict_confirms_known_stock(self):
        self.assertEqual(
            backup.verdict_for(backup.EXPECTED_SIZE, backup.EXPECTED_SHA256, True),
            "VERIFIED STOCK",
        )

    def test_verdict_accepts_matching_hook_sites(self):
        self.assertEqual(
            backup.verdict_for(backup.EXPECTED_SIZE, "0" * 64, True),
            "PATCH-COMPATIBLE LAYOUT",
        )

    def test_verdict_keeps_custom_layout_as_snapshot(self):
        self.assertEqual(
            backup.verdict_for(backup.EXPECTED_SIZE, "0" * 64, False),
            "DEVICE SNAPSHOT (unrecognized layout)",
        )

    def test_verdict_keeps_unexpected_size_as_snapshot(self):
        self.assertEqual(
            backup.verdict_for(1, backup.EXPECTED_SHA256, True),
            "DEVICE SNAPSHOT (unrecognized layout)",
        )

    def test_hook_sites_match_planted_layout(self):
        data = bytearray(backup.EXPECTED_SIZE)
        for offset, expected in backup.EXPECTED_HOOK_SITES.items():
            data[offset:offset + len(expected)] = expected
        self.assertTrue(backup.hook_sites_match(bytes(data)))

    def test_hook_sites_reject_single_changed_byte(self):
        data = bytearray(backup.EXPECTED_SIZE)
        for offset, expected in backup.EXPECTED_HOOK_SITES.items():
            data[offset:offset + len(expected)] = expected
        data[0x8A20] ^= 0xFF
        self.assertFalse(backup.hook_sites_match(bytes(data)))

    def test_hook_sites_reject_short_buffer(self):
        self.assertFalse(backup.hook_sites_match(b"too short"))

    def test_layout_problems_accept_planted_image(self):
        data = bytearray(build_direct_led.EXPECTED_BACKUP_SIZE)
        for offset, expected in build_direct_led.EXPECTED_HOOK_SITES.items():
            data[offset:offset + len(expected)] = expected
        for offset, expected in build_direct_led.EXPECTED_IMPL_PREFIXES.items():
            data[offset:offset + len(expected)] = expected
        self.assertEqual(build_direct_led.layout_problems(bytes(data)), [])

    def test_layout_problems_name_wrong_hook_site(self):
        data = bytearray(build_direct_led.EXPECTED_BACKUP_SIZE)
        for offset, expected in build_direct_led.EXPECTED_HOOK_SITES.items():
            data[offset:offset + len(expected)] = expected
        for offset, expected in build_direct_led.EXPECTED_IMPL_PREFIXES.items():
            data[offset:offset + len(expected)] = expected
        data[0x65B8] ^= 0xFF
        problems = build_direct_led.layout_problems(bytes(data))
        self.assertEqual(len(problems), 1)
        self.assertIn("0x080085b8", problems[0])

    def test_layout_problems_flag_wrong_jump_target(self):
        data = bytearray(build_direct_led.EXPECTED_BACKUP_SIZE)
        for offset, expected in build_direct_led.EXPECTED_HOOK_SITES.items():
            data[offset:offset + len(expected)] = expected
        problems = build_direct_led.layout_problems(bytes(data))
        self.assertTrue(any("jump target" in problem for problem in problems))

    def test_handler_area_erased(self):
        self.assertTrue(build_direct_led.handler_area_erased(bytearray(b"\xff" * 60000), 100))
        dirty = bytearray(b"\xff" * 60000)
        dirty[0x0800FDC0 - 0x08002000 + 50] = 0x00
        self.assertFalse(build_direct_led.handler_area_erased(dirty, 100))

    def test_compare_reports_identical_reference(self):
        data = bytes(100)
        self.assertEqual(
            backup.compare_to_reference(data, bytes(100), "ref.bin"),
            "IDENTICAL TO REFERENCE (ref.bin)",
        )

    def test_compare_reports_size_mismatch(self):
        self.assertIn(
            "reads are 10 bytes, reference is 20 bytes",
            backup.compare_to_reference(bytes(10), bytes(20), "ref.bin"),
        )

    def test_compare_reports_intact_patch_region(self):
        ref = bytearray(60000)
        reads = bytearray(ref)
        reads[59000] = 1
        reads[59999] = 2
        message = backup.compare_to_reference(bytes(reads), bytes(ref), "ref.bin")
        self.assertIn("2 of 60000 bytes differ", message)
        self.assertIn("patch region intact", message)

    def test_compare_reports_differing_patch_region(self):
        ref = bytearray(60000)
        reads = bytearray(ref)
        reads[100] = 1
        message = backup.compare_to_reference(bytes(reads), bytes(ref), "ref.bin")
        self.assertIn("patch region DIFFERS", message)
        self.assertNotIn("normal after boot", message)


if __name__ == "__main__":
    unittest.main()
