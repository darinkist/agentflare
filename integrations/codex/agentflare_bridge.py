#!/usr/bin/env python3
"""Thin executable entry point with the install lock before package import."""

import fcntl
import os
import sys
from contextlib import contextmanager
from pathlib import Path


LOCK_NAME = ".agentflare-codex-install.lock"


@contextmanager
def runtime_install_lock():
    target = Path(sys.argv[0]).resolve().parent
    package = target / "agentflare_codex"
    lock = target / LOCK_NAME
    if not package.is_dir() or not lock.exists():
        yield
        return
    descriptor = os.open(lock, os.O_RDONLY)
    with os.fdopen(descriptor, "r") as handle:
        fcntl.flock(handle.fileno(), fcntl.LOCK_SH)
        try:
            yield
        finally:
            fcntl.flock(handle.fileno(), fcntl.LOCK_UN)


def main() -> int:
    with runtime_install_lock():
        try:
            from agentflare_codex.cli import main as run
        except ModuleNotFoundError:
            sys.path.insert(0, str(Path(__file__).resolve().parent / "src"))
            from agentflare_codex.cli import main as run
        return run()


if __name__ == "__main__":
    raise SystemExit(main())
