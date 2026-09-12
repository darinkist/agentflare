"""Runtime state, atomic files, locks, and freshness markers."""

import fcntl
import hashlib
import json
import os
import secrets
import tempfile
import time
from contextlib import contextmanager
from pathlib import Path
from typing import Iterator, Optional

from . import events
from .config import (
    MAX_SAFE_INTEGER,
    MAX_SUBAGENTS,
    SOURCE_FRESHNESS_GRACE as DEFAULT_SOURCE_FRESHNESS_GRACE,
    STATE_DIR as DEFAULT_STATE_DIR,
)


# Tests and controlled installations may replace this boundary explicitly.
STATE_DIR = DEFAULT_STATE_DIR
SOURCE_FRESHNESS_GRACE = DEFAULT_SOURCE_FRESHNESS_GRACE
MAIN_AGENT_ID = events.MAIN_AGENT_ID
ACTIVE_STATES = events.ACTIVE_STATES
TERMINAL_STATES = events.TERMINAL_STATES
ALL_MAIN_STATES = ACTIVE_STATES | TERMINAL_STATES
STATE_FIELDS = {
    "session_id", "display_session_id", "bind_id", "binding_id", "next_seq",
    "needs_sync", "transaction_uncertain", "worker_pid", "worker_token",
    "main", "subagents", "next_subagent_start_order",
}


def read_json(path: str) -> Optional[dict]:
    try:
        with open(path, encoding="utf-8") as handle:
            value = json.load(handle)
    except (FileNotFoundError, OSError, ValueError, UnicodeError):
        return None
    return value if isinstance(value, dict) else None


def write_json(path: str, value: dict, prefix: str) -> None:
    destination = Path(path)
    destination.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    descriptor, temporary = tempfile.mkstemp(prefix=prefix, dir=destination.parent)
    try:
        os.fchmod(descriptor, 0o600)
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            json.dump(value, handle, separators=(",", ":"), ensure_ascii=True)
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, destination)
    except BaseException:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass
        raise


def acquire_exclusive_lock(path: str):
    """Acquire a lock for callers that explicitly release the handle."""
    destination = Path(path)
    destination.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    descriptor = os.open(destination, os.O_RDWR | os.O_CREAT, 0o600)
    os.fchmod(descriptor, 0o600)
    handle = os.fdopen(descriptor, "r+")
    fcntl.flock(handle.fileno(), fcntl.LOCK_EX)
    return handle


@contextmanager
def exclusive_lock(path: str) -> Iterator[object]:
    handle = acquire_exclusive_lock(path)
    try:
        yield handle
    finally:
        fcntl.flock(handle.fileno(), fcntl.LOCK_UN)
        handle.close()


@contextmanager
def locked_state(value: str) -> Iterator[object]:
    """Hold the session lock for one coordinated section.

    Acquire locked_active() first when both locks are needed, never the
    other way round.
    """
    with exclusive_lock(lock_path(value)) as handle:
        yield handle


@contextmanager
def locked_active() -> Iterator[object]:
    """Hold the active-session lock for one coordinated section."""
    with exclusive_lock(active_session_lock_path()) as handle:
        yield handle


def ensure_state_dir() -> None:
    os.makedirs(STATE_DIR, mode=0o700, exist_ok=True)


def session_key(value: str) -> str:
    return hashlib.sha256(value.encode("utf-8", "surrogatepass")).hexdigest()[:32]


def state_path(value: str) -> str:
    return os.path.join(STATE_DIR, f"{session_key(value)}.json")


def lock_path(value: str) -> str:
    return os.path.join(STATE_DIR, f"{session_key(value)}.lock")


def stop_path(value: str) -> str:
    return os.path.join(STATE_DIR, f"{session_key(value)}.stop")


def worker_start_path(value: str) -> str:
    return os.path.join(STATE_DIR, f"{session_key(value)}.worker-start")


def source_seen_path(value: str) -> str:
    return os.path.join(STATE_DIR, f"{session_key(value)}.source-seen")


def active_session_path() -> str:
    return os.path.join(STATE_DIR, "active-session.json")


def active_session_lock_path() -> str:
    return os.path.join(STATE_DIR, "active-session.lock")


def mark_source_observed_locked(value: str) -> None:
    """Touch the source-seen marker. Callers must hold the active lock,
    then the session lock (see locked_active/locked_state)."""
    ensure_state_dir()
    destination = source_seen_path(value)
    descriptor = os.open(destination, os.O_WRONLY | os.O_CREAT, 0o600)
    try:
        os.fchmod(descriptor, 0o600)
        os.utime(destination, None)
    finally:
        os.close(descriptor)


def mark_source_observed(value: str) -> None:
    """Record freshness with the bridge's active-lock then session-lock order."""
    with locked_active():
        with locked_state(value):
            mark_source_observed_locked(value)


def source_is_fresh(value: str, now: Optional[float] = None) -> bool:
    try:
        observed_at = os.stat(source_seen_path(value)).st_mtime
    except OSError:
        return False
    age = (time.time() if now is None else now) - observed_at
    return age <= SOURCE_FRESHNESS_GRACE


def _valid_main_state(main: object) -> bool:
    if main is None:
        return True
    if not isinstance(main, dict) or set(main) != {
        "agent_id", "run_id", "state", "turn_id", "run_generation"
    }:
        return False
    return (
        main.get("agent_id") == MAIN_AGENT_ID
        and events.valid_identifier(main.get("run_id"))
        and main.get("state") in ALL_MAIN_STATES
        and events.valid_identifier(main.get("turn_id"))
        and type(main.get("run_generation")) is int
        and 0 <= main["run_generation"] <= MAX_SAFE_INTEGER
    )


def _valid_subagent_record(record: object) -> bool:
    if not isinstance(record, dict) or set(record) != {
        "agent_id", "run_id", "parent_agent_id", "state", "start_order"
    }:
        return False
    return (
        events.valid_identifier(record.get("agent_id"))
        and events.valid_identifier(record.get("run_id"))
        and events.valid_identifier(record.get("parent_agent_id"))
        and record.get("state") in ACTIVE_STATES
        and type(record.get("start_order")) is int
        and 0 < record["start_order"] <= MAX_SAFE_INTEGER
    )


def valid_state(state: object, value: str) -> bool:
    if not isinstance(state, dict) or set(state) != STATE_FIELDS:
        return False
    if not events.valid_local_string(value) or state.get("session_id") != value:
        return False
    if state.get("display_session_id") != events.compact_session_id(value):
        return False
    if not events.valid_identifier(state.get("bind_id")):
        return False
    if not events.valid_identifier(state.get("binding_id"), allow_empty=True):
        return False
    if type(state.get("next_seq")) is not int or not 0 <= state["next_seq"] <= MAX_SAFE_INTEGER:
        return False
    if type(state.get("needs_sync")) is not bool or type(state.get("transaction_uncertain")) is not bool:
        return False
    if type(state.get("worker_pid")) is not int or state["worker_pid"] < 0:
        return False
    if not events.valid_identifier(state.get("worker_token"), allow_empty=True):
        return False
    if state["binding_id"] and state["next_seq"] < 1:
        return False
    if not state["binding_id"] and not state["needs_sync"]:
        return False
    if not _valid_main_state(state.get("main")):
        return False
    subagents = state.get("subagents")
    if not isinstance(subagents, dict) or len(subagents) > MAX_SUBAGENTS:
        return False
    seen_runs = set()
    seen_orders = set()
    for key, record in subagents.items():
        if not events.valid_identifier(key) or not _valid_subagent_record(record):
            return False
        run_key = (record["agent_id"], record["run_id"])
        if run_key in seen_runs or record["start_order"] in seen_orders:
            return False
        seen_runs.add(run_key)
        seen_orders.add(record["start_order"])
    next_order = state.get("next_subagent_start_order")
    return (
        type(next_order) is int
        and 0 < next_order <= MAX_SAFE_INTEGER
        and (not seen_orders or next_order > max(seen_orders))
    )


def new_bind_id(value: str) -> str:
    return f"codex-hook-{session_key(value)[:16]}-{secrets.token_hex(4)}"


def initial_state(value: str, bind_id: Optional[str] = None) -> dict:
    state = {
        "session_id": value,
        "display_session_id": events.compact_session_id(value),
        "bind_id": bind_id or new_bind_id(value),
        "binding_id": "",
        "next_seq": 0,
        "needs_sync": True,
        "transaction_uncertain": False,
        "worker_pid": 0,
        "worker_token": "",
        "main": None,
        "subagents": {},
        "next_subagent_start_order": 1,
    }
    if not valid_state(state, value):
        raise ValueError("unable to create valid bridge state")
    return state


def reset_desired_state(state: dict) -> None:
    """Clear the desired display snapshot, keeping session and binding identity."""
    state["main"] = None
    state["subagents"] = {}
    state["next_subagent_start_order"] = 1
    state["needs_sync"] = True


def reset_binding(state: dict) -> None:
    """Forget the service binding, keeping the desired snapshot for replay.

    An unbound state must always need a sync; valid_state enforces the pairing.
    """
    state["binding_id"] = ""
    state["next_seq"] = 0
    state["needs_sync"] = True


def load_state(value: str) -> Optional[dict]:
    state = read_json(state_path(value))
    return state if valid_state(state, value) else None


def save_state(state: dict) -> None:
    if not isinstance(state, dict) or not valid_state(state, state.get("session_id")):
        raise ValueError("invalid bridge state")
    write_json(state_path(state["session_id"]), state, ".display-state.")


def load_active_session() -> Optional[str]:
    state = read_json(active_session_path())
    value = state.get("session_id") if isinstance(state, dict) else None
    return value if events.valid_local_string(value) else None


def save_active_session(value: str) -> None:
    write_json(active_session_path(), {"session_id": value}, ".active-session.")


def clear_active_session() -> None:
    try:
        os.unlink(active_session_path())
    except FileNotFoundError:
        pass


def known_local_sessions() -> list[str]:
    sessions = []
    try:
        paths = Path(STATE_DIR).glob("*.json")
    except OSError:
        return sessions
    for path in paths:
        if path.name == "active-session.json":
            continue
        state = read_json(str(path))
        if isinstance(state, dict) and events.valid_local_string(state.get("session_id")):
            sessions.append(state["session_id"])
    active = load_active_session()
    if active and active not in sessions:
        sessions.append(active)
    return sessions


def resolve_local_session(service_value: object) -> Optional[str]:
    if not isinstance(service_value, str) or not service_value:
        return None
    for candidate in known_local_sessions():
        if events.compact_session_id(candidate) == service_value or candidate == service_value:
            return candidate
    return None
