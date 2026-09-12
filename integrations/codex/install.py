#!/usr/bin/env python3
"""Safely install or remove the AgentFlare-owned Codex hook package."""

from __future__ import annotations

import argparse
import copy
import fcntl
import hashlib
import json
import os
import shlex
import shutil
import stat
import subprocess
import sys
import tempfile
from collections.abc import Iterator
from contextlib import contextmanager, suppress
from pathlib import Path

ROOT = Path(__file__).resolve().parent
CANONICAL_HOOKS = ROOT / "hooks.json"
BRIDGE = ROOT / "agentflare_bridge.py"
PACKAGE_ROOT = ROOT / "src" / "agentflare_codex"
PACKAGE_FILES = (
    "__init__.py",
    "cli.py",
    "config.py",
    "events.py",
    "storage.py",
    "protocol.py",
    "session.py",
    "worker.py",
)
STATE_NAME = ".agentflare-codex-install.json"
LOCK_NAME = ".agentflare-codex-install.lock"
MARKER = "agentflare_bridge.py"
STATE_VERSION = 2


def sha256_bytes(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def sha256_file(path: Path) -> str:
    return sha256_bytes(path.read_bytes())


def load_json(path: Path, default: dict) -> dict:
    if not path.exists():
        return copy.deepcopy(default)
    if path.is_symlink() or not path.is_file():
        raise ValueError(f"{path} is not a regular file")
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as error:
        raise ValueError(f"invalid JSON file {path}: {error.msg}") from error
    if not isinstance(value, dict):
        raise ValueError(f"invalid JSON file {path}: expected an object")
    return value


def load_config(path: Path) -> dict:
    value = load_json(path, {"hooks": {}})
    hooks = value.setdefault("hooks", {})
    if not isinstance(hooks, dict):
        raise ValueError(f"invalid hook configuration {path}: hooks must be an object")
    for event, groups in hooks.items():
        if not isinstance(event, str) or not isinstance(groups, list):
            raise ValueError(f"invalid hook configuration {path}: hooks.{event} must be an array")
        for group in groups:
            if not isinstance(group, dict) or not isinstance(group.get("hooks"), list):
                raise ValueError(f"invalid hook configuration {path}: every group needs a hooks array")
            if any(not isinstance(hook, dict) for hook in group["hooks"]):
                raise ValueError(f"invalid hook configuration {path}: every hook must be an object")
    return value


def atomic_write(path: Path, content: bytes, mode: int = 0o600) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    try:
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(content)
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(temporary, mode)
        os.replace(temporary, path)
    except BaseException:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass
        raise


def render_json(value: dict) -> bytes:
    return (json.dumps(value, indent=2, sort_keys=True) + "\n").encode("utf-8")


def base_interpreter() -> Path:
    """Return a supported non-venv interpreter safe for installed hooks."""
    candidate = Path(getattr(sys, "_base_executable", sys.executable)).resolve()
    try:
        result = subprocess.run(
            [str(candidate), "-c", "import sys; print(sys.version_info[:2]); print(sys.prefix == sys.base_prefix)"],
            capture_output=True, check=False, text=True, timeout=1,
        )
    except (OSError, subprocess.SubprocessError) as error:
        raise ValueError("unable to determine a permanent Python interpreter") from error
    lines = result.stdout.splitlines()
    if result.returncode != 0 or len(lines) != 2 or lines[1] != "True":
        raise ValueError("unable to determine a permanent Python interpreter")
    try:
        major, minor = (int(value.strip()) for value in lines[0].strip("()").split(","))
    except ValueError as error:
        raise ValueError("permanent Python interpreter is invalid") from error
    if (major, minor) < (3, 12) or not candidate.is_file() or not os.access(candidate, os.X_OK):
        raise ValueError("Python 3.12 or newer is required for installed hooks")
    return candidate


def command_for(target: Path, interpreter: Path) -> str:
    return (
        f"{shlex.quote(str(interpreter))} -B "
        f"{shlex.quote(str((target / 'agentflare_bridge.py').resolve()))}"
    )


def configured_hooks(target: Path, interpreter: Path) -> dict:
    canonical = load_config(CANONICAL_HOOKS)
    hooks = copy.deepcopy(canonical["hooks"])
    for groups in hooks.values():
        for group in groups:
            for hook in group["hooks"]:
                hook["command"] = command_for(target, interpreter)
    return hooks


def handler_records(hooks: dict) -> list[dict]:
    return [
        {"event": event, "command": hook["command"]}
        for event, groups in hooks.items()
        for group in groups
        for hook in group["hooks"]
    ]


def has_unrecognized_agentflare_command(config: dict) -> bool:
    return any(
        MARKER in str(hook.get("command", ""))
        for groups in config["hooks"].values()
        for group in groups
        for hook in group["hooks"]
    )


def source_files() -> dict[str, Path]:
    files = {"agentflare_bridge.py": BRIDGE}
    for name in PACKAGE_FILES:
        source = PACKAGE_ROOT / name
        if source.is_symlink() or not source.is_file():
            raise ValueError(f"missing managed package source {source}")
        files[f"agentflare_codex/{name}"] = source
    return files


def _valid_handlers(value: object) -> bool:
    return isinstance(value, list) and all(
        isinstance(record, dict) and set(record) == {"event", "command"}
        and isinstance(record["event"], str) and isinstance(record["command"], str)
        for record in value
    )


def _state_v2(value: dict, target: Path) -> bool:
    if set(value) != {"version", "target", "files", "handlers"} or value.get("version") != STATE_VERSION:
        return False
    if value.get("target") != str(target.resolve()) or not isinstance(value.get("files"), dict):
        return False
    return (
        set(value["files"]) == set(source_files())
        and all(isinstance(digest, str) and len(digest) == 64 for digest in value["files"].values())
        and _valid_handlers(value.get("handlers"))
    )


def load_state(path: Path, target: Path) -> dict | None:
    if not path.exists():
        return None
    value = load_json(path, {})
    if _state_v2(value, target):
        return value
    raise ValueError(f"invalid AgentFlare installation state {path}")


def locate_handlers(config: dict, records: list[dict]) -> list[tuple[str, int, int]]:
    locations = []
    available = [
        (event, group_index, hook_index, hook.get("command"))
        for event, groups in config["hooks"].items()
        for group_index, group in enumerate(groups)
        for hook_index, hook in enumerate(group["hooks"])
    ]
    for record in records:
        matches = [
            (event, group_index, hook_index)
            for event, group_index, hook_index, command in available
            if event == record["event"] and command == record["command"]
        ]
        if len(matches) != 1:
            raise ValueError("installed AgentFlare handlers were changed or are ambiguous; refusing to modify them")
        locations.append(matches[0])
        available = [entry for entry in available if entry[:3] != matches[0]]
    return locations


def remove_tracked_handlers(config: dict, records: list[dict]) -> dict:
    result = copy.deepcopy(config)
    by_event: dict[str, dict[int, set[int]]] = {}
    for event, group_index, hook_index in locate_handlers(config, records):
        by_event.setdefault(event, {}).setdefault(group_index, set()).add(hook_index)
    for event, groups in by_event.items():
        current = result["hooks"][event]
        for group_index in sorted(groups, reverse=True):
            group = current[group_index]
            group["hooks"] = [hook for index, hook in enumerate(group["hooks"]) if index not in groups[group_index]]
            if not group["hooks"]:
                del current[group_index]
        if not current:
            del result["hooks"][event]
    return result


def state_for(target: Path, hooks: dict) -> dict:
    return {
        "version": STATE_VERSION,
        "target": str(target.resolve()),
        "files": {relative: sha256_file(source) for relative, source in source_files().items()},
        "handlers": handler_records(hooks),
    }


def _package_entries(package: Path) -> Iterator[Path]:
    """Yield installed package entries, rejecting symlinks and unknown caches."""
    for path in package.rglob("*"):
        if path.is_symlink():
            raise ValueError("installed AgentFlare package contains a symlink")
        if "__pycache__" in path.parts:
            if path.is_dir() and path.name != "__pycache__":
                raise ValueError("installed AgentFlare package contains an unrecognized cache directory")
            if path.is_file() and path.suffix not in {".pyc", ".pyo"}:
                raise ValueError("installed AgentFlare package contains an unrecognized cache file")
        yield path


def _validate_package_contents(target: Path, expected: set[str]) -> None:
    package = target / "agentflare_codex"
    if not package.exists():
        if expected:
            raise ValueError("installed AgentFlare package is missing")
        return
    if package.is_symlink() or not package.is_dir():
        raise ValueError("installed AgentFlare package is not a regular directory")
    actual = {
        path.relative_to(target).as_posix()
        for path in _package_entries(package)
        if path.is_file() and "__pycache__" not in path.parts
    }
    if actual != expected:
        raise ValueError("installed AgentFlare package contains unrecognized files; refusing to modify it")


def _known_cache_files(target: Path) -> set[Path]:
    package = target / "agentflare_codex"
    if not package.is_dir() or package.is_symlink():
        return set()
    return {
        path
        for path in _package_entries(package)
        if path.is_file() and "__pycache__" in path.parts
    }


def _remove_empty_cache_dirs(target: Path) -> None:
    package = target / "agentflare_codex"
    if not package.is_dir() or package.is_symlink():
        return
    for path in sorted(package.rglob("__pycache__"), reverse=True):
        if path.is_dir() and not path.is_symlink():
            with suppress(OSError):
                path.rmdir()


def validate_owned_installation(config: dict, state: dict, target: Path) -> None:
    locate_handlers(config, state["handlers"])
    expected = set(state["files"])
    package_files = {path for path in expected if path.startswith("agentflare_codex/")}
    _validate_package_contents(target, package_files)
    for relative, digest in state["files"].items():
        path = target / relative
        if path.is_symlink() or not path.is_file() or sha256_file(path) != digest:
            raise ValueError("installed AgentFlare component was changed or is missing; refusing to modify it")


def backup(path: Path) -> Path:
    candidate = path.with_name(path.name + ".agentflare-backup")
    index = 1
    while candidate.exists():
        candidate = path.with_name(path.name + f".agentflare-backup.{index}")
        index += 1
    shutil.copy2(path, candidate)
    return candidate


def apply_transaction(writes: dict[Path, bytes], deletes: set[Path]) -> None:
    paths = list(writes) + list(deletes)
    snapshots = {path: (path.read_bytes(), stat.S_IMODE(path.stat().st_mode)) if path.exists() else None for path in paths}
    try:
        for path, content in writes.items():
            atomic_write(path, content)
        for path in deletes:
            path.unlink()
    except OSError:
        for path, snapshot in snapshots.items():
            if snapshot is None:
                try:
                    path.unlink()
                except FileNotFoundError:
                    pass
            else:
                atomic_write(path, snapshot[0], snapshot[1])
        raise


@contextmanager
def install_lock(target: Path):
    target.mkdir(parents=True, exist_ok=True)
    descriptor = os.open(target / LOCK_NAME, os.O_RDWR | os.O_CREAT, 0o600)
    with os.fdopen(descriptor, "r+") as handle:
        try:
            fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as error:
            raise ValueError("Codex hook or installer is active; close Codex before updating") from error
        try:
            yield
        finally:
            fcntl.flock(handle.fileno(), fcntl.LOCK_UN)


def target_directory(args: argparse.Namespace) -> Path:
    if args.mode == "global":
        if args.project:
            raise ValueError("--project can only be used with --mode project")
        return Path(args.codex_home or os.environ.get("CODEX_HOME", Path.home() / ".codex")).resolve()
    if args.codex_home:
        raise ValueError("--codex-home can only be used with --mode global")
    if not args.project:
        raise ValueError("--project is required for project mode")
    project = Path(args.project).resolve()
    if not project.is_dir():
        raise ValueError(f"project directory does not exist: {project}")
    return project / ".codex"


def remove_installation(args: argparse.Namespace, target: Path, config: dict, state: dict | None) -> int:
    config_path = target / "hooks.json"
    state_path = target / STATE_NAME
    if state is None:
        print("no AgentFlare installation is registered; nothing changed")
        return 0
    validate_owned_installation(config, state, target)
    desired = remove_tracked_handlers(config, state["handlers"])
    if args.preview:
        print(render_json(desired).decode("utf-8"), end="")
        return 0
    tracked = state["files"]
    backup_path = backup(config_path) if desired != config else None
    try:
        apply_transaction({config_path: render_json(desired)}, {target / relative for relative in tracked} | {state_path})
        for cache_file in _known_cache_files(target):
            cache_file.unlink()
        _remove_empty_cache_dirs(target)
        package = target / "agentflare_codex"
        if package.exists() and not any(package.iterdir()):
            package.rmdir()
    except OSError:
        if backup_path:
            backup_path.unlink(missing_ok=True)
        raise
    print(f"removed AgentFlare hooks from {config_path}")
    return 0


def perform_installation(args: argparse.Namespace, target: Path, config: dict, state: dict | None) -> int:
    config_path = target / "hooks.json"
    state_path = target / STATE_NAME
    interpreter = base_interpreter()
    desired_hooks = configured_hooks(target, interpreter)
    desired_state = state_for(target, desired_hooks)
    if state is not None:
        validate_owned_installation(config, state, target)
        if not args.update and state == desired_state:
            print(f"AgentFlare hooks already installed in {config_path}")
            return 0
        base = remove_tracked_handlers(config, state["handlers"])
    else:
        if (target / "agentflare_bridge.py").exists() or (target / "agentflare_codex").exists():
            raise ValueError("AgentFlare component exists but is not AgentFlare-owned")
        base = copy.deepcopy(config)
    desired = copy.deepcopy(base)
    for event, groups in desired_hooks.items():
        desired["hooks"].setdefault(event, []).extend(groups)
    if args.preview:
        print(render_json(desired).decode("utf-8"), end="")
        return 0
    backup_path = backup(config_path) if config_path.exists() and desired != config else None
    # Prepare and write every managed runtime file before publishing the
    # hook configuration that can invoke it. apply_transaction restores
    # these files and the configuration together if a later write fails.
    writes = {target / relative: source.read_bytes() for relative, source in source_files().items()}
    writes[config_path] = render_json(desired)
    writes[state_path] = render_json(desired_state)
    try:
        apply_transaction(writes, _known_cache_files(target))
        _remove_empty_cache_dirs(target)
    except OSError:
        if backup_path:
            backup_path.unlink(missing_ok=True)
        raise
    print(f"installed AgentFlare hooks in {config_path}")
    return 0


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--mode", choices=("global", "project"), default="global")
    parser.add_argument("--codex-home")
    parser.add_argument("--project")
    parser.add_argument("--preview", action="store_true")
    parser.add_argument("--update", action="store_true")
    parser.add_argument("--remove", action="store_true")
    args = parser.parse_args()
    if args.remove and args.update:
        parser.error("--remove and --update cannot be combined")

    target = target_directory(args)
    with install_lock(target):
        config = load_config(target / "hooks.json")
        state = load_state(target / STATE_NAME, target)
        if state is None and has_unrecognized_agentflare_command(config):
            raise ValueError("unrecognized AgentFlare-like hook exists; refusing to take ownership")
        if args.remove:
            return remove_installation(args, target, config, state)
        return perform_installation(args, target, config, state)


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, ValueError) as error:
        print(f"hook installation failed: {error}", file=sys.stderr)
        raise SystemExit(1)
