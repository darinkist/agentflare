"""Worker identity and wait primitives used by the session coordinator."""

import base64
import contextlib
from enum import Enum
import os
import signal
import subprocess
import sys
import threading
import time


class WorkerStatus(str, Enum):
    """Result of a locked worker ownership and freshness check."""

    KEEP_RUNNING = "KEEP_RUNNING"
    EXPIRED = "EXPIRED"
    NOT_OWNER = "NOT_OWNER"


def worker_matches(pid: object, token: object, valid_identifier) -> bool:
    """Return whether a PID still names the worker launched with this token."""
    if type(pid) is not int or pid <= 0 or not valid_identifier(token):
        return False
    try:
        completed = subprocess.run(
            ["ps", "-p", str(pid), "-o", "command="],
            check=False,
            capture_output=True,
            text=True,
            timeout=0.25,
        )
    except (OSError, subprocess.SubprocessError):
        return False
    return completed.returncode == 0 and "--worker" in completed.stdout and token in completed.stdout


def encode_session(value: str, text_bytes) -> str:
    return "b64:" + base64.urlsafe_b64encode(text_bytes(value)).decode("ascii")


def decode_session(value: str, valid_local_string) -> str | None:
    if not value.startswith("b64:"):
        return value if valid_local_string(value) else None
    try:
        decoded = base64.urlsafe_b64decode(value[4:] + "=" * (-len(value[4:]) % 4))
        result = decoded.decode("utf-8", "surrogatepass")
    except (ValueError, UnicodeError):
        return None
    return result if valid_local_string(result) else None


def wait_for_next_heartbeat(stop_file: str, duration: float, stop_event) -> bool:
    """Wait interruptibly without treating a stop file as a signal to write."""
    if os.path.exists(stop_file):
        return False
    if stop_event.wait(duration):
        return False
    return not os.path.exists(stop_file)


def wait_for_start_registration(
    start_file: str, duration: float, stop_event, *, clock=time.monotonic
) -> bool:
    """Wait for parent registration, but never wait on a stale gate forever."""
    deadline = clock() + duration
    while os.path.exists(start_file):
        remaining = deadline - clock()
        if remaining <= 0:
            return False
        if stop_event.wait(min(0.05, remaining)):
            return False
    return True


def start(entry_script: str, encoded_session: str, token: str):
    """Launch a detached worker after the session coordinator created its gate."""
    return subprocess.Popen(
        [sys.executable, "-B", entry_script, "--worker", encoded_session, token],
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        close_fds=True,
        start_new_session=True,
    )


def terminate(worker, timeout: float) -> None:
    """Reap a worker whose ownership could not be persisted."""
    try:
        worker.terminate()
    except OSError:
        return
    for _ in range(2):
        try:
            worker.wait(timeout=timeout)
            return
        except subprocess.TimeoutExpired:
            with contextlib.suppress(OSError):
                worker.kill()
        except OSError:
            return


def signal_stop(pid: int) -> None:
    try:
        os.kill(pid, signal.SIGTERM)
    except ProcessLookupError:
        pass


def run(wait, step) -> int:
    """Own process signals and waiting while delegating one coordinated step."""
    stop_event = threading.Event()

    def terminate(_signum, _frame):
        stop_event.set()

    signal.signal(signal.SIGTERM, terminate)
    signal.signal(signal.SIGINT, terminate)
    while wait(stop_event):
        result = step()
        if result is not None:
            return result
    return 0
