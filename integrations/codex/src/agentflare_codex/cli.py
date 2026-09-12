"""Process input, diagnostics, and passive Codex hook responses."""

from __future__ import annotations

import argparse
import fcntl
import json
import os
import sys
import time

from . import events, protocol, session, storage
from .config import MAX_LOG_BYTES as DEFAULT_MAX_LOG_BYTES
from .config import OUTPUT_PATH as DEFAULT_OUTPUT_PATH


VALUE_FIELDS = events.VALUE_FIELDS
PRESENCE_FIELDS = events.PRESENCE_FIELDS
OUTPUT_PATH = DEFAULT_OUTPUT_PATH
MAX_LOG_BYTES = DEFAULT_MAX_LOG_BYTES


def _lock_log_file(descriptor: int) -> None:
    fcntl.flock(descriptor, fcntl.LOCK_EX)


def record(
    payload: dict,
    display_state: str | None,
    delivery: str,
    delivery_elapsed_ms: int | None = None,
    delivery_failure: str | None = None,
    bridge_event: str | None = None,
) -> None:
    """Write only allowlisted operational metadata to the local hook log.

    The log keeps exactly one capped file: under an exclusive lock, an entry
    that would exceed MAX_LOG_BYTES first discards the previous content, and
    a single entry larger than the cap is rejected unwritten.
    """
    event = {
        "observed_at": time.strftime("%Y-%m-%dT%H:%M:%S%z", time.localtime()),
        "present_fields": sorted(field for field in PRESENCE_FIELDS if field in payload),
        "display_delivery": delivery,
    }
    if display_state is not None:
        event["display_state"] = display_state
    if delivery_elapsed_ms is not None:
        event["display_delivery_elapsed_ms"] = delivery_elapsed_ms
    if delivery_failure is not None:
        event["display_delivery_failure"] = delivery_failure
    if bridge_event is not None:
        event["bridge_event"] = bridge_event
    for field in VALUE_FIELDS:
        value = payload.get(field)
        if isinstance(value, (str, int, float, bool)):
            event[field] = value

    encoded = (
        json.dumps(event, separators=(",", ":"), sort_keys=True) + "\n"
    ).encode("utf-8", "backslashreplace")
    parent = os.path.dirname(OUTPUT_PATH)
    if parent:
        os.makedirs(parent, mode=0o700, exist_ok=True)
    descriptor = os.open(OUTPUT_PATH, os.O_WRONLY | os.O_APPEND | os.O_CREAT, 0o600)
    try:
        os.fchmod(descriptor, 0o600)
        _lock_log_file(descriptor)
        if len(encoded) > MAX_LOG_BYTES:
            return
        if os.fstat(descriptor).st_size + len(encoded) > MAX_LOG_BYTES:
            os.ftruncate(descriptor, 0)
        view = memoryview(encoded)
        while view:
            written = os.write(descriptor, view)
            if written <= 0:
                raise OSError("unable to append hook log")
            view = view[written:]
    finally:
        os.close(descriptor)


def record_best_effort(
    payload: dict,
    display_state: str | None,
    delivery: str,
    delivery_elapsed_ms: int | None = None,
    delivery_failure: str | None = None,
    bridge_event: str | None = None,
) -> bool:
    try:
        record(
            payload,
            display_state,
            delivery,
            delivery_elapsed_ms,
            delivery_failure,
            bridge_event,
        )
    except OSError as error:
        try:
            print(f"hook bridge logging failed: {protocol.error_code(error)}", file=sys.stderr)
        except OSError:
            pass
        return False
    return True


def main() -> int:
    session.ENTRY_SCRIPT = os.path.abspath(sys.argv[0])
    arguments = parse_args()
    if arguments.worker:
        if not arguments.worker_session:
            raise ValueError("worker session is required")
        if not events.valid_identifier(arguments.worker_token):
            raise ValueError("worker token is required")
        worker_session = session.decode_worker_session(arguments.worker_session)
        if worker_session is None:
            raise ValueError("worker session is invalid")
        return session.worker_main(
            worker_session,
            arguments.worker_token,
            diagnostic=record_best_effort,
        )
    if arguments.recover:
        if not events.valid_local_string(arguments.session_id):
            raise ValueError("--session-id is required with --recover")
        if arguments.worker_session is not None or arguments.worker_token is not None:
            raise ValueError("unexpected positional argument")
        session.recover_uncertain_session(arguments.session_id)
        print(
            f"recovered uncertain AgentFlare session "
            f"{events.safe_identifier(arguments.session_id, 'session')}"
        )
        return 0
    if arguments.session_id is not None:
        raise ValueError("--session-id requires --recover")
    payload = json.load(sys.stdin)
    if not isinstance(payload, dict):
        raise ValueError("hook payload must be a JSON object")
    return handle_payload(payload)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--worker", action="store_true")
    parser.add_argument("--recover", action="store_true")
    parser.add_argument("--session-id")
    parser.add_argument("worker_session", nargs="?")
    parser.add_argument("worker_token", nargs="?")
    return parser.parse_args()


def _write_stop_response() -> None:
    sys.stdout.write('{"continue":true}\n')
    sys.stdout.flush()


def handle_payload(payload: dict) -> int:
    """Process one Codex hook without exposing prompts or transcript data."""
    event_name = payload.get("hook_event_name")
    current_session = events.session_id(payload)
    mapped_display = events.display_state_for(payload)
    display_state = None
    delivery = "not_applicable"
    delivery_elapsed_ms = None
    delivery_failure = None
    bridge_event = None
    started_at = time.monotonic()
    try:
        if event_name == "SessionStart":
            storage.mark_source_observed(current_session)
            delivery, bridge_event = "accepted", "session_start"
        elif event_name == "SessionEnd":
            session.deactivate_session(current_session)
            delivery, bridge_event = "accepted", "session_end"
        elif event_name == "SubagentStart":
            display_state, bridge_event = "running", "subagent_start"
            session.send_subagent_start(payload, diagnostic=record_best_effort)
            delivery = "accepted"
        elif event_name == "SubagentStop":
            bridge_event = "subagent_stop"
            delivery = "not_observed" if session.send_subagent_stop(
                payload, diagnostic=record_best_effort
            ) == "unknown" else "accepted"
            if delivery == "not_observed":
                bridge_event = "subagent_stop_unknown"
        elif mapped_display is not None:
            display_state, reason = mapped_display
            bridge_event = "main_completion_candidate" if event_name == "Stop" else None
            session.send_display_state(
                payload,
                display_state,
                reason,
                diagnostic=record_best_effort,
            )
            delivery = "accepted"
        else:
            storage.mark_source_observed(current_session)
    except (OSError, protocol.ServiceError, ValueError) as error:
        delivery, delivery_failure = "failed", protocol.error_code(error)
        if event_name == "SubagentStart" and delivery_failure == "capacity_exceeded":
            bridge_event = "subagent_capacity_exceeded"
        elif (
            isinstance(event_name, str)
            and event_name in {"SubagentStart", "SubagentStop"}
            and delivery_failure == "request_too_large"
        ):
            bridge_event = "subagent_sync_oversized"
        print(f"hook bridge service delivery failed: {delivery_failure}", file=sys.stderr)
    finally:
        if mapped_display is not None or event_name in {
            "SessionStart", "SessionEnd", "SubagentStart", "SubagentStop"
        }:
            delivery_elapsed_ms = round((time.monotonic() - started_at) * 1000)
        try:
            record_best_effort(
                payload,
                display_state,
                delivery,
                delivery_elapsed_ms,
                delivery_failure,
                bridge_event,
            )
        finally:
            if event_name in {"Stop", "SubagentStop"}:
                _write_stop_response()
    return 0


__all__ = ["handle_payload", "main", "parse_args", "record", "record_best_effort"]
