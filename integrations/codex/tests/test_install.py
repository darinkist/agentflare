#!/usr/bin/env python3

import importlib.util
import fcntl
import json
import os
import shlex
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock


ROOT = Path(__file__).resolve().parents[1]
PATH = ROOT / "install.py"
SPEC = importlib.util.spec_from_file_location("codex_install", PATH)
assert SPEC is not None and SPEC.loader is not None
installer = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(installer)


class HookInstallerTest(unittest.TestCase):
    def command(self, home: Path, *arguments: str) -> list[str]:
        return [sys.executable, str(PATH), "--mode", "global", "--codex-home", str(home), *arguments]

    def test_shell_safe_commands_and_idempotent_install(self):
        with tempfile.TemporaryDirectory(prefix="agent flare; $home ") as directory:
            home = Path(directory) / "Codex Home & Hooks"
            first = subprocess.run(self.command(home), capture_output=True, text=True)
            self.assertEqual(first.returncode, 0, first.stderr)
            config = json.loads((home / "hooks.json").read_text(encoding="utf-8"))
            command = config["hooks"]["Stop"][0]["hooks"][0]["command"]
            self.assertIn("'", command)
            self.assertEqual(
                shlex.split(command),
                [
                    str(installer.base_interpreter()),
                    "-B",
                    str((home / "agentflare_bridge.py").resolve()),
                ],
            )
            self.assertTrue((home / "agentflare_codex" / "session.py").is_file())
            second = subprocess.run(self.command(home), capture_output=True, text=True)
            self.assertEqual(second.returncode, 0, second.stderr)
            self.assertIn("already installed", second.stdout)
            self.assertFalse((home / "hooks.json.agentflare-backup").exists())

    def test_mixed_group_preserved_when_removing_owned_handler(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            foreign = {"type": "command", "command": "echo unrelated", "timeout": 7}
            config = {"hooks": {"Stop": [{"matcher": "*", "hooks": [foreign]}]}}
            (home / "hooks.json").write_text(json.dumps(config), encoding="utf-8")
            installed = subprocess.run(self.command(home), capture_output=True, text=True)
            self.assertEqual(installed.returncode, 0, installed.stderr)
            changed = json.loads((home / "hooks.json").read_text(encoding="utf-8"))
            changed["hooks"]["Stop"][0]["hooks"].append(changed["hooks"]["Stop"][1]["hooks"][0])
            del changed["hooks"]["Stop"][1]
            (home / "hooks.json").write_text(json.dumps(changed), encoding="utf-8")
            removed = subprocess.run(self.command(home, "--remove"), capture_output=True, text=True)
            self.assertEqual(removed.returncode, 0, removed.stderr)
            self.assertEqual(json.loads((home / "hooks.json").read_text(encoding="utf-8")), config)
            second = subprocess.run(self.command(home, "--remove"), capture_output=True, text=True)
            self.assertEqual(second.returncode, 0, second.stderr)
            self.assertIn("nothing changed", second.stdout)

    def test_foreign_bridge_and_unregistered_removal_are_untouched(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            home.mkdir(exist_ok=True)
            bridge = home / "agentflare_bridge.py"
            bridge.write_text("foreign bridge", encoding="utf-8")
            config = home / "hooks.json"
            config.write_text(json.dumps({"hooks": {"Stop": [{"hooks": [{"command": "echo unrelated"}]}]}}), encoding="utf-8")
            before = {path: path.read_bytes() for path in (bridge, config)}
            failed = subprocess.run(self.command(home), capture_output=True, text=True)
            self.assertNotEqual(failed.returncode, 0)
            removed = subprocess.run(self.command(home, "--remove"), capture_output=True, text=True)
            self.assertEqual(removed.returncode, 0, removed.stderr)
            self.assertEqual({path: path.read_bytes() for path in before}, before)

    def test_changed_handler_or_bridge_refuses_update_and_removal(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            self.assertEqual(subprocess.run(self.command(home), capture_output=True).returncode, 0)
            config = home / "hooks.json"
            installed_config = config.read_bytes()
            changed = json.loads(config.read_text(encoding="utf-8"))
            changed["hooks"]["Stop"][0]["hooks"][0]["command"] = "echo changed"
            config.write_text(json.dumps(changed), encoding="utf-8")
            before = config.read_bytes()
            for action in ("--update", "--remove"):
                result = subprocess.run(self.command(home, action), capture_output=True, text=True)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(config.read_bytes(), before)
            config.write_bytes(installed_config)
            # A changed bridge is also never overwritten or removed.
            (home / "agentflare_bridge.py").write_text("changed", encoding="utf-8")
            result = subprocess.run(self.command(home, "--remove"), capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual((home / "agentflare_bridge.py").read_text(encoding="utf-8"), "changed")

    def test_write_failure_restores_existing_files(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            config = home / "hooks.json"
            original = b'{"hooks":{"Stop":[{"hooks":[{"command":"echo unrelated"}]}]}}\n'
            config.write_bytes(original)
            real_write = installer.atomic_write
            calls = 0

            def fail_second(path, content, mode=0o600):
                nonlocal calls
                calls += 1
                if calls == 2:
                    raise OSError("simulated write failure")
                real_write(path, content, mode)

            with mock.patch.object(installer, "atomic_write", side_effect=fail_second):
                with self.assertRaisesRegex(OSError, "simulated"):
                    installer.apply_transaction(
                        {config: b"changed", home / "bridge": b"new"}, set()
                    )
            self.assertEqual(config.read_bytes(), original)
            self.assertFalse((home / "bridge").exists())

    def test_project_mode_and_ambiguous_arguments_are_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            project = Path(directory) / "project with spaces"
            project.mkdir()
            result = subprocess.run(
                [sys.executable, str(PATH), "--mode", "project", "--project", str(project)], capture_output=True, text=True
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertTrue((project / ".codex" / "hooks.json").is_file())
            for arguments in (
                ["--mode", "project"],
                ["--mode", "global", "--project", str(project)],
                ["--mode", "project", "--project", str(project), "--codex-home", str(project)],
            ):
                rejected = subprocess.run([sys.executable, str(PATH), *arguments], capture_output=True, text=True)
                self.assertNotEqual(rejected.returncode, 0)

    def test_invalid_configuration_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "hooks.json"
            path.write_text("[]", encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "expected an object"):
                installer.load_config(path)

            path.write_text('{"hooks":null}\n', encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "hooks must be an object"):
                installer.load_config(path)

    def test_empty_object_configuration_is_normalized_and_installed(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            (home / "hooks.json").write_text("{}\n", encoding="utf-8")
            installed = subprocess.run(self.command(home), capture_output=True, text=True)
            self.assertEqual(installed.returncode, 0, installed.stderr)
            config = json.loads((home / "hooks.json").read_text(encoding="utf-8"))
            self.assertIn("hooks", config)
            self.assertIn("Stop", config["hooks"])

    def test_installed_hook_runs_outside_repository(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory) / "codex-home"
            installed = subprocess.run(self.command(home), capture_output=True, text=True)
            self.assertEqual(installed.returncode, 0, installed.stderr)
            config = json.loads((home / "hooks.json").read_text(encoding="utf-8"))
            command = shlex.split(config["hooks"]["SubagentStop"][0]["hooks"][0]["command"])
            foreign = Path(directory) / "foreign-working-directory"
            foreign.mkdir()
            environment = {
                "AGENTFLARE_CODEX_HOOK_STATE_DIR": str(Path(directory) / "state"),
                "AGENTFLARE_CODEX_HOOK_LOG": str(Path(directory) / "events.jsonl"),
            }
            completed = subprocess.run(
                command,
                cwd=foreign,
                env=environment,
                input=json.dumps({
                    "hook_event_name": "SubagentStop", "session_id": "session-a",
                    "turn_id": "turn-a", "agent_id": "agent-a", "agent_type": "worker",
                }),
                text=True,
                capture_output=True,
                check=False,
            )
            self.assertEqual(completed.returncode, 0, completed.stderr)
            self.assertEqual(completed.stdout, '{"continue":true}\n')
            self.assertFalse((home / "agentflare_codex" / "__pycache__").exists())

    def test_known_bytecode_cache_is_removed_but_foreign_package_files_are_not(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            self.assertEqual(subprocess.run(self.command(home), capture_output=True).returncode, 0)
            cache = home / "agentflare_codex" / "__pycache__"
            cache.mkdir()
            (cache / "cli.cpython-312.pyc").write_bytes(b"cache")
            updated = subprocess.run(self.command(home, "--update"), capture_output=True, text=True)
            self.assertEqual(updated.returncode, 0, updated.stderr)
            self.assertFalse(cache.exists())

            cache.mkdir()
            (cache / "foreign.txt").write_text("foreign", encoding="utf-8")
            refused = subprocess.run(self.command(home, "--update"), capture_output=True, text=True)
            self.assertNotEqual(refused.returncode, 0)
            self.assertEqual((cache / "foreign.txt").read_text(encoding="utf-8"), "foreign")

    def test_version_one_state_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            self.assertEqual(subprocess.run(self.command(home), capture_output=True).returncode, 0)
            state_path = home / installer.STATE_NAME
            current = json.loads(state_path.read_text(encoding="utf-8"))
            legacy = {
                "version": 1,
                "target": str(home.resolve()),
                "bridge_sha256": "0" * 64,
                "handlers": current["handlers"],
            }
            state_path.write_text(json.dumps(legacy), encoding="utf-8")
            refused = subprocess.run(
                self.command(home, "--update"), capture_output=True, text=True
            )
            self.assertNotEqual(refused.returncode, 0)
            self.assertIn("invalid AgentFlare installation state", refused.stderr)

    def test_extra_or_modified_package_file_refuses_update_and_remove(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            self.assertEqual(subprocess.run(self.command(home), capture_output=True).returncode, 0)
            extra = home / "agentflare_codex" / "unrecognized.py"
            extra.write_text("foreign", encoding="utf-8")
            for action in ("--update", "--remove"):
                result = subprocess.run(self.command(home, action), capture_output=True, text=True)
                self.assertNotEqual(result.returncode, 0)
            self.assertEqual(extra.read_text(encoding="utf-8"), "foreign")

    def test_active_installation_lock_refuses_update(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            self.assertEqual(subprocess.run(self.command(home), capture_output=True).returncode, 0)
            with (home / installer.LOCK_NAME).open("r") as handle:
                fcntl.flock(handle.fileno(), fcntl.LOCK_SH)
                result = subprocess.run(self.command(home, "--update"), capture_output=True, text=True)
                self.assertNotEqual(result.returncode, 0)
                fcntl.flock(handle.fileno(), fcntl.LOCK_UN)


if __name__ == "__main__":
    unittest.main()
