"""Coordinate session ownership, desired display state, and worker actions."""

from __future__ import annotations

import copy
import contextlib
import os
import secrets
import time
from functools import partial
from typing import Callable, Optional

from . import events, protocol, storage, worker
from .config import (
    HEARTBEAT_INTERVAL,
    MAX_SAFE_INTEGER,
    MAX_SUBAGENTS,
    WORKER_START_TIMEOUT,
)


SOURCE = events.SOURCE
MAIN_AGENT_ID = events.MAIN_AGENT_ID
ACTIVE_STATES = events.ACTIVE_STATES
TERMINAL_STATES = events.TERMINAL_STATES
ENTRY_SCRIPT = os.path.abspath(__file__)
ServiceError = protocol.ServiceError
WorkerStatus = worker.WorkerStatus

Diagnostic = Callable[..., bool]


def _null_diagnostic(*args: object, **kwargs: object) -> bool:
    return True


def request(payload: dict) -> dict:
    """Use the configured protocol boundary for one service request."""
    return protocol.request(
        payload,
        encode=protocol.encode_request,
        socket_path=protocol.SOCKET_PATH,
        timeout=protocol.SOCKET_TIMEOUT,
        max_request_bytes=protocol.MAX_REQUEST_BYTES,
        max_response_bytes=protocol.MAX_RESPONSE_BYTES,
        safe_identifier=events.safe_identifier,
    )


def is_uncertain_error(error: Exception) -> bool:
    return protocol.is_uncertain(error)


def error_code(error: Exception) -> str:
    return protocol.error_code(error)


def _replace_state(state: dict, replacement: dict) -> None:
    state.clear()
    state.update(replacement)


def bind(value: str, previous: Optional[dict] = None) -> dict:
    state = copy.deepcopy(previous) if previous is not None else storage.initial_state(value)
    if state.get("session_id") != value:
        state = storage.initial_state(value)
    state["display_session_id"] = events.compact_session_id(value)
    try:
        result = request(
            {
                "version": 2,
                "type": "display_bind",
                "source": SOURCE,
                "session_id": state["display_session_id"],
                "bind_id": state["bind_id"],
                "takeover": True,
            }
        )
    except (OSError, ServiceError) as error:
        state["transaction_uncertain"] = is_uncertain_error(error)
        state["needs_sync"] = True
        storage.save_state(state)
        raise
    binding_id = result.get("binding_id")
    next_seq = result.get("next_seq")
    if not events.valid_identifier(binding_id) or not protocol.validate_sequence(next_seq):
        state["needs_sync"] = True
        storage.save_state(state)
        raise ServiceError("service_invalid_binding")
    state["binding_id"] = binding_id
    state["next_seq"] = next_seq
    state["needs_sync"] = True
    state["transaction_uncertain"] = False
    storage.save_state(state)
    return state


def _send_transaction(
    state: dict,
    payload: dict,
    accepted: dict,
    *,
    force_synced: bool = True,
    advance_sequence: bool = True,
) -> None:
    protocol.send_transaction(
        state,
        payload,
        accepted,
        request=request,
        save_state=storage.save_state,
        replace_state=_replace_state,
        force_synced=force_synced,
        advance_sequence=advance_sequence,
    )


def ensure_ready(value: str, diagnostic: Diagnostic = _null_diagnostic) -> dict:
    state = storage.load_state(value)
    if state is not None and state["transaction_uncertain"]:
        raise ServiceError("service_transaction_uncertain", transaction_uncertain=True)
    if state is None:
        if os.path.exists(storage.state_path(value)):
            diagnostic(
                {"hook_event_name": "StateRecovery", "session_id": value},
                None,
                "degraded",
                bridge_event="state_reset",
            )
        return bind_and_sync(value)
    if state["needs_sync"]:
        return bind_and_sync(value, state)
    return state


def send_sync_once(state: dict, candidate: Optional[dict] = None) -> None:
    desired = copy.deepcopy(candidate) if candidate is not None else copy.deepcopy(state)
    _send_transaction(
        state,
        events.sync_request(state, desired),
        desired,
    )


def bind_and_sync(value: str, previous: Optional[dict] = None) -> dict:
    state = bind(value, previous)
    try:
        send_sync_once(state)
    except (OSError, ServiceError, ValueError) as error:
        state["needs_sync"] = True
        state["transaction_uncertain"] = is_uncertain_error(error)
        storage.save_state(state)
        raise
    return state


def bound_session() -> Optional[str]:
    result = request({"version": 2, "type": "status"})
    if result.get("source") != SOURCE:
        return None
    return storage.resolve_local_session(result.get("session_id"))


def _release_session_locked(value: str) -> None:
    with storage.locked_state(value):
        state = storage.load_state(value)
        _stop_worker_locked(value, state)
        _unbind_locked(value, state)


def _activate_session_locked(value: str) -> None:
    previous = storage.load_active_session()
    if previous == value:
        return
    service_session = bound_session()
    to_release = {
        candidate
        for candidate in (previous, service_session)
        if candidate and candidate != value
    }
    for candidate in to_release:
        _release_session_locked(candidate)
    storage.save_active_session(value)


def activate_session(value: str) -> None:
    with storage.locked_active():
        _activate_session_locked(value)


def _clear_desired_state_locked(state: Optional[dict]) -> None:
    if state is None:
        return
    storage.reset_desired_state(state)
    storage.save_state(state)


def _clear_source_marker(value: str) -> None:
    with contextlib.suppress(FileNotFoundError):
        os.unlink(storage.source_seen_path(value))


def deactivate_session(value: str) -> None:
    with storage.locked_active():
        with storage.locked_state(value):
            state = storage.load_state(value)
            _stop_worker_locked(value, state)
            if storage.load_active_session() == value:
                _unbind_locked(value, state)
                storage.clear_active_session()
            _clear_desired_state_locked(storage.load_state(value))
            _clear_source_marker(value)


def recover(value: str, state: dict) -> dict:
    storage.reset_binding(state)
    storage.save_state(state)
    return bind_and_sync(value, state)


def recover_uncertain_session(value: str) -> dict:
    with storage.locked_active():
        with storage.locked_state(value):
            state = storage.load_state(value)
            if state is None or not state["transaction_uncertain"]:
                raise ServiceError("service_recovery_not_required")
            state["transaction_uncertain"] = False
            storage.reset_binding(state)
            state["bind_id"] = storage.new_bind_id(value)
            storage.save_state(state)
            return bind_and_sync(value, state)


def _raise_if_transaction_uncertain(value: str) -> None:
    state = storage.load_state(value)
    if state is not None and state["transaction_uncertain"]:
        raise ServiceError("service_transaction_uncertain", transaction_uncertain=True)


def _display_event_request(
    state: dict,
    display_state: str,
    payload: dict,
    reason: Optional[str],
    event_run_id: Optional[str] = None,
) -> dict:
    event = {
        "version": 2,
        "type": "display_event",
        "binding_id": state["binding_id"],
        "seq": state["next_seq"],
        "role": "main",
        "agent_id": MAIN_AGENT_ID,
        "run_id": event_run_id or events.run_id(payload),
        "state": display_state,
    }
    if reason is not None:
        event["reason"] = reason
    return event


def _send_event_once(state: dict, display_state: str, payload: dict, reason: Optional[str]) -> None:
    event_run_id, turn, run_generation = events.main_run_for_event(
        state, payload, display_state
    )
    event = _display_event_request(state, display_state, payload, reason, event_run_id)
    candidate = copy.deepcopy(state)
    candidate["main"] = {
        "agent_id": MAIN_AGENT_ID,
        "run_id": event["run_id"],
        "state": display_state,
        "turn_id": turn,
        "run_generation": run_generation,
    }
    _send_transaction(state, event, candidate)


def _send_main_with_bootstrap(
    state: dict, display_state: str, payload: dict, reason: Optional[str]
) -> None:
    try:
        _send_event_once(state, display_state, payload, reason)
    except (OSError, ServiceError) as error:
        if (
            error_code(error) == "unknown_agent"
            and state.get("main") is None
            and display_state in TERMINAL_STATES
        ):
            _send_event_once(state, "running", payload, None)
            _send_event_once(state, display_state, payload, reason)
            return
        raise


def _send_with_recovery(value: str, state: dict, send_once: Callable[[dict], object]) -> object:
    """Send once, recovering the binding a single time when the service forgot it."""
    try:
        outcome = send_once(state)
    except (OSError, ServiceError) as error:
        if error_code(error) != "unknown_binding":
            state["transaction_uncertain"] = is_uncertain_error(error)
            storage.save_state(state)
            raise
        state = recover(value, state)
        outcome = send_once(state)
    _start_worker_locked(value, state)
    return outcome


def _transact(
    value: str,
    send_once: Callable[[dict], object],
    *,
    diagnostic: Diagnostic = _null_diagnostic,
    precheck: Optional[Callable[[], object]] = None,
) -> object:
    """Lock, precheck, activate, then observe, ready the session and send.

    precheck runs under both locks before activation and the source mark, so
    an "unknown" outcome skips activation and any service call. It may return
    a final outcome to skip the write, or raise.
    """
    with storage.locked_active():
        if precheck is not None:
            with storage.locked_state(value):
                outcome = precheck()
                if outcome is not None:
                    return outcome
        _activate_session_locked(value)
        with storage.locked_state(value):
            storage.mark_source_observed_locked(value)
            state = ensure_ready(value, diagnostic)
            return _send_with_recovery(value, state, send_once)


def send_display_state(
    payload: dict,
    desired_state: str,
    reason: Optional[str],
    *,
    diagnostic: Diagnostic = _null_diagnostic,
) -> None:
    current_session = events.session_id(payload)
    _raise_if_transaction_uncertain(current_session)
    _transact(
        current_session,
        partial(
            _send_main_with_bootstrap,
            display_state=desired_state,
            payload=payload,
            reason=reason,
        ),
        diagnostic=diagnostic,
    )


def _subagent_record(session: str, turn: str, agent: str, start_order: int) -> dict:
    return {
        "agent_id": events.compact_agent_id(session, agent),
        "run_id": events.compact_run_id(session, agent, turn, start_order),
        "parent_agent_id": MAIN_AGENT_ID,
        "state": "running",
        "start_order": start_order,
    }


def _subagent_event_request(state: dict, record: dict) -> dict:
    return {
        "version": 2,
        "type": "display_event",
        "binding_id": state["binding_id"],
        "seq": state["next_seq"],
        "role": "subagent",
        "agent_id": record["agent_id"],
        "run_id": record["run_id"],
        "parent_agent_id": record["parent_agent_id"],
        "state": record["state"],
    }


def _send_subagent_start_once(state: dict, session: str, turn: str, agent: str) -> str:
    key = events.local_agent_key(agent)
    if key in state["subagents"]:
        return "duplicate"
    if len(state["subagents"]) >= MAX_SUBAGENTS:
        raise ServiceError("capacity_exceeded")
    start_order = state["next_subagent_start_order"]
    if start_order >= MAX_SAFE_INTEGER:
        raise ServiceError("capacity_exceeded")
    record = _subagent_record(session, turn, agent, start_order)
    candidate = copy.deepcopy(state)
    candidate["subagents"][key] = record
    candidate["next_subagent_start_order"] = start_order + 1
    _send_transaction(state, _subagent_event_request(state, record), candidate)
    return "accepted"


def send_subagent_start(payload: dict, *, diagnostic: Diagnostic = _null_diagnostic) -> str:
    session, turn, agent, _agent_type = events.required_subagent_fields(payload)
    _raise_if_transaction_uncertain(session)
    key = events.local_agent_key(agent)
    preexisting = storage.load_state(session)
    if preexisting is not None and key not in preexisting["subagents"] and (
        len(preexisting["subagents"]) >= MAX_SUBAGENTS
        or preexisting["next_subagent_start_order"] >= MAX_SAFE_INTEGER
    ):
        raise ServiceError("capacity_exceeded")
    return _transact(
        session,
        partial(_send_subagent_start_once, session=session, turn=turn, agent=agent),
        diagnostic=diagnostic,
    )


def _matching_subagent_key(state: dict, session: str, turn: str, agent: str) -> Optional[str]:
    """Return the active key only when it belongs to this exact run.

    A late stop from an older turn must not match the newer run of a reused
    agent ID, so the stored run ID has to equal the run ID derived from the
    stop's own turn and the record's start order.
    """
    key = events.local_agent_key(agent)
    record = state.get("subagents", {}).get(key)
    if not isinstance(record, dict):
        return None
    expected = events.compact_run_id(session, agent, turn, record.get("start_order"))
    if record.get("run_id") != expected:
        return None
    return key


def _send_subagent_stop_once(state: dict, session: str, turn: str, agent: str) -> str:
    key = _matching_subagent_key(state, session, turn, agent)
    if key is None:
        return "unknown"
    candidate = copy.deepcopy(state)
    del candidate["subagents"][key]
    send_sync_once(state, candidate)
    return "accepted"


def send_subagent_stop(payload: dict, *, diagnostic: Diagnostic = _null_diagnostic) -> str:
    session, turn, agent, _agent_type = events.required_subagent_fields(payload)

    def precheck() -> Optional[str]:
        initial = storage.load_state(session)
        if initial is None or _matching_subagent_key(initial, session, turn, agent) is None:
            return "unknown"
        if initial["transaction_uncertain"]:
            raise ServiceError("service_transaction_uncertain", transaction_uncertain=True)
        return None

    return _transact(
        session,
        partial(_send_subagent_stop_once, session=session, turn=turn, agent=agent),
        diagnostic=diagnostic,
        precheck=precheck,
    )


def _worker_owned(
    state: Optional[dict], worker_token: Optional[str], *, require_token: bool = False
) -> bool:
    return (
        state is not None
        and state.get("worker_pid") == os.getpid()
        and (not require_token or worker_token is not None)
        and (worker_token is None or state.get("worker_token") == worker_token)
    )


def _worker_status_locked(current_session: str, worker_token: Optional[str]) -> WorkerStatus:
    if storage.load_active_session() != current_session:
        return WorkerStatus.NOT_OWNER
    state = storage.load_state(current_session)
    if not _worker_owned(state, worker_token, require_token=True):
        return WorkerStatus.NOT_OWNER
    return WorkerStatus.KEEP_RUNNING if storage.source_is_fresh(current_session) else WorkerStatus.EXPIRED


def worker_status(current_session: str, worker_token: Optional[str]) -> WorkerStatus:
    with storage.locked_active():
        with storage.locked_state(current_session):
            return _worker_status_locked(current_session, worker_token)


def _send_heartbeat_locked(current_session: str, worker_token: Optional[str]) -> bool:
    state = storage.load_state(current_session)
    if worker_token is not None and not _worker_owned(state, worker_token):
        return False
    state = ensure_ready(current_session)
    heartbeat = {
        "version": 2,
        "type": "display_heartbeat",
        "binding_id": state["binding_id"],
        "seq": state["next_seq"],
    }
    try:
        _send_transaction(state, heartbeat, copy.deepcopy(state))
    except (OSError, ServiceError) as error:
        state = storage.load_state(current_session)
        if state is not None and error_code(error) == "unknown_binding":
            recover(current_session, state)
            return True
        if state is not None and is_uncertain_error(error):
            state["transaction_uncertain"] = True
            storage.save_state(state)
        raise
    return True


def send_heartbeat(current_session: str, worker_token: Optional[str] = None) -> bool:
    with storage.locked_active():
        if storage.load_active_session() != current_session:
            return False
        with storage.locked_state(current_session):
            return _send_heartbeat_locked(current_session, worker_token)


def _cleanup_expired_locked(current_session: str, state: dict) -> None:
    state["worker_pid"] = 0
    state["worker_token"] = ""
    storage.save_state(state)
    if storage.load_active_session() == current_session:
        _unbind_locked(current_session, state)
        storage.clear_active_session()
    _clear_desired_state_locked(storage.load_state(current_session))
    _clear_source_marker(current_session)


def _worker_step_locked(current_session: str, worker_token: Optional[str]) -> WorkerStatus:
    if storage.load_active_session() != current_session:
        return WorkerStatus.NOT_OWNER
    state = storage.load_state(current_session)
    if not _worker_owned(state, worker_token, require_token=True):
        return WorkerStatus.NOT_OWNER
    if storage.source_is_fresh(current_session):
        _send_heartbeat_locked(current_session, worker_token)
        return WorkerStatus.KEEP_RUNNING
    _cleanup_expired_locked(current_session, state)
    return WorkerStatus.EXPIRED


def _worker_step(current_session: str, worker_token: Optional[str]) -> WorkerStatus:
    """Decide ownership, freshness, and the follow-up under one lock pair."""
    with storage.locked_active():
        with storage.locked_state(current_session):
            return _worker_step_locked(current_session, worker_token)


def deactivate_stale_session(current_session: str, worker_token: Optional[str]) -> bool:
    return _worker_step(current_session, worker_token) is WorkerStatus.EXPIRED


def worker_matches(pid: object, token: object) -> bool:
    return worker.worker_matches(pid, token, events.valid_identifier)


def _worker_session_token(value: str) -> str:
    return worker.encode_session(value, events.text_bytes)


def decode_worker_session(value: str) -> Optional[str]:
    return worker.decode_session(value, events.valid_local_string)


def _start_worker_locked(current_session: str, state: dict) -> None:
    stop_file = storage.stop_path(current_session)
    storage.ensure_state_dir()
    with contextlib.suppress(FileNotFoundError):
        os.unlink(stop_file)
    if state["transaction_uncertain"]:
        return
    if worker_matches(state.get("worker_pid", 0), state.get("worker_token", "")):
        return
    token = f"worker-{secrets.token_hex(16)}"
    start_file = storage.worker_start_path(current_session)
    descriptor = os.open(start_file, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    os.close(descriptor)
    try:
        process = worker.start(ENTRY_SCRIPT, _worker_session_token(current_session), token)
    except (OSError, ValueError):
        with contextlib.suppress(FileNotFoundError):
            os.unlink(start_file)
        return
    state["worker_pid"] = process.pid
    state["worker_token"] = token
    try:
        storage.save_state(state)
        os.unlink(start_file)
    except (OSError, ValueError):
        state["worker_pid"] = 0
        state["worker_token"] = ""
        try:
            storage.save_state(state)
        except (OSError, ValueError):
            pass
        with contextlib.suppress(FileNotFoundError):
            os.unlink(start_file)
        worker.terminate(process, 0.25)


def start_worker(current_session: str) -> None:
    with storage.locked_active():
        with storage.locked_state(current_session):
            state = storage.load_state(current_session) or storage.initial_state(current_session)
            _start_worker_locked(current_session, state)


def _stop_worker_locked(current_session: str, state: Optional[dict]) -> None:
    storage.ensure_state_dir()
    descriptor = os.open(storage.stop_path(current_session), os.O_WRONLY | os.O_CREAT, 0o600)
    os.close(descriptor)
    if state is None:
        return
    if worker_matches(state.get("worker_pid", 0), state.get("worker_token", "")):
        worker.signal_stop(state["worker_pid"])
    state["worker_pid"] = 0
    state["worker_token"] = ""
    storage.save_state(state)


def stop_worker(current_session: str) -> None:
    with storage.locked_active():
        with storage.locked_state(current_session):
            _stop_worker_locked(current_session, storage.load_state(current_session))


def _unbind_locked(current_session: str, state: Optional[dict]) -> None:
    if state is None or state["transaction_uncertain"] or not state["binding_id"]:
        return
    try:
        request_payload = {
            "version": 2,
            "type": "display_unbind",
            "binding_id": state["binding_id"],
            "seq": state["next_seq"],
        }
        candidate = copy.deepcopy(state)
        storage.reset_binding(candidate)
        _send_transaction(
            state,
            request_payload,
            candidate,
            force_synced=False,
            advance_sequence=False,
        )
    except (OSError, ServiceError) as error:
        if error_code(error) == "unknown_binding":
            storage.reset_binding(state)
            storage.save_state(state)
            return
        state["transaction_uncertain"] = is_uncertain_error(error)
        storage.save_state(state)
        raise


def unbind(current_session: str) -> None:
    with storage.locked_active():
        with storage.locked_state(current_session):
            _unbind_locked(current_session, storage.load_state(current_session))


def worker_main(
    current_session: str,
    worker_token: Optional[str] = None,
    diagnostic: Diagnostic = _null_diagnostic,
) -> int:
    def wait(stop_event) -> bool:
        if not worker.wait_for_start_registration(
            storage.worker_start_path(current_session),
            WORKER_START_TIMEOUT,
            stop_event,
        ):
            return False
        return worker.wait_for_next_heartbeat(
            storage.stop_path(current_session), HEARTBEAT_INTERVAL, stop_event
        )

    def step() -> Optional[int]:
        started_at = time.monotonic()
        try:
            status = _worker_step(current_session, worker_token)
        except (OSError, ServiceError, ValueError) as error:
            diagnostic(
                {"hook_event_name": "SessionHeartbeat", "session_id": current_session},
                None,
                "failed",
                round((time.monotonic() - started_at) * 1000),
                error_code(error),
                bridge_event="display_heartbeat",
            )
            return 1 if is_uncertain_error(error) else None
        if status is WorkerStatus.NOT_OWNER:
            return 0
        if status is WorkerStatus.EXPIRED:
            diagnostic(
                {"hook_event_name": "SessionFreshness", "session_id": current_session},
                None,
                "stale",
                round((time.monotonic() - started_at) * 1000),
                bridge_event="source_stale",
            )
            return 0
        diagnostic(
            {"hook_event_name": "SessionHeartbeat", "session_id": current_session},
            None,
            "accepted",
            round((time.monotonic() - started_at) * 1000),
            bridge_event="display_heartbeat",
        )
        return None

    return worker.run(wait, step)
