"""Bounded V2 transport and the persisted sequenced transaction boundary."""

import copy
import json
import socket

from .config import MAX_REQUEST_BYTES as DEFAULT_MAX_REQUEST_BYTES
from .config import MAX_RESPONSE_BYTES as DEFAULT_MAX_RESPONSE_BYTES
from .config import MAX_SAFE_INTEGER, SOCKET_PATH as DEFAULT_SOCKET_PATH
from .config import SOCKET_TIMEOUT as DEFAULT_SOCKET_TIMEOUT
from .events import valid_identifier


SOCKET_PATH = DEFAULT_SOCKET_PATH
SOCKET_TIMEOUT = DEFAULT_SOCKET_TIMEOUT
MAX_REQUEST_BYTES = DEFAULT_MAX_REQUEST_BYTES
MAX_RESPONSE_BYTES = DEFAULT_MAX_RESPONSE_BYTES
ID_FIELDS = {
    "source", "session_id", "bind_id", "binding_id", "agent_id", "run_id",
    "parent_agent_id",
}


class ServiceError(RuntimeError):
    """A bounded AgentFlare service request failed."""

    def __init__(self, code: str, *, transaction_uncertain: bool = False):
        self.code = code
        self.transaction_uncertain = transaction_uncertain
        super().__init__(code)


def _validate_payload(payload: dict) -> None:
    if not isinstance(payload, dict):
        raise ServiceError("invalid_request")
    if payload.get("version") != 2:
        return
    for field in ID_FIELDS:
        if field in payload and payload[field] is not None and not valid_identifier(payload[field]):
            raise ServiceError("invalid_identifier")
    if payload.get("type") != "display_sync":
        return
    main = payload.get("main")
    if isinstance(main, dict):
        for field in ("agent_id", "run_id"):
            if not valid_identifier(main.get(field)):
                raise ServiceError("invalid_identifier")
    subagents = payload.get("subagents")
    if not isinstance(subagents, list):
        return
    for snapshot in subagents:
        if not isinstance(snapshot, dict):
            raise ServiceError("invalid_identifier")
        for field in ("agent_id", "run_id", "parent_agent_id"):
            if not valid_identifier(snapshot.get(field)):
                raise ServiceError("invalid_identifier")


def encode_request(payload: dict, max_request_bytes: int = MAX_REQUEST_BYTES) -> bytes:
    """Encode and size-check one complete newline-delimited request."""
    _validate_payload(payload)
    try:
        encoded = (
            json.dumps(
                payload,
                ensure_ascii=False,
                allow_nan=False,
                separators=(",", ":"),
                sort_keys=True,
            )
            + "\n"
        ).encode("utf-8")
    except (TypeError, UnicodeError, ValueError) as error:
        raise ServiceError("invalid_request") from error
    if len(encoded) > max_request_bytes:
        raise ServiceError("request_too_large")
    return encoded


def request(
    payload: dict,
    *,
    encode=encode_request,
    socket_path: str = SOCKET_PATH,
    timeout: float = SOCKET_TIMEOUT,
    max_request_bytes: int = MAX_REQUEST_BYTES,
    max_response_bytes: int = MAX_RESPONSE_BYTES,
    safe_identifier,
) -> dict:
    """Send one request and preserve ambiguity after a started write."""
    encoded = encode(payload, max_request_bytes)
    write_started = False
    response = bytearray()
    try:
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
            client.settimeout(timeout)
            client.connect(socket_path)
            write_started = True
            client.sendall(encoded)
            while len(response) <= max_response_bytes:
                chunk = client.recv(4096)
                if not chunk:
                    break
                response.extend(chunk)
                if b"\n" in chunk:
                    break
    except socket.timeout as error:
        if write_started:
            raise ServiceError("service_transaction_uncertain", transaction_uncertain=True) from error
        raise ServiceError("socket_timeout") from error
    except OSError as error:
        if write_started:
            raise ServiceError("service_transaction_uncertain", transaction_uncertain=True) from error
        raise

    raw = bytes(response)
    if not raw:
        raise ServiceError("service_no_response", transaction_uncertain=write_started)
    line, separator, _ = raw.partition(b"\n")
    if separator:
        complete = len(line) + 1 <= max_response_bytes
    else:
        complete = len(raw) <= max_response_bytes
    if not complete:
        raise ServiceError("service_response_too_large", transaction_uncertain=write_started)
    try:
        envelope = json.loads(line)
    except (TypeError, ValueError) as error:
        raise ServiceError("service_invalid_response", transaction_uncertain=write_started) from error
    if not isinstance(envelope, dict):
        raise ServiceError("service_invalid_response", transaction_uncertain=write_started)
    if not envelope.get("ok"):
        error = envelope.get("error")
        code = error.get("code") if isinstance(error, dict) else None
        raise ServiceError(safe_identifier(code, "service_rejected"))
    result = envelope.get("result")
    if not isinstance(result, dict):
        raise ServiceError("service_invalid_result", transaction_uncertain=write_started)
    if payload.get("type") in {"display_event", "display_sync", "display_heartbeat", "display_unbind"} and result.get("accepted_seq") != payload.get("seq"):
        raise ServiceError("service_invalid_accepted_seq", transaction_uncertain=write_started)
    return result


def is_uncertain(error: Exception) -> bool:
    return isinstance(error, ServiceError) and error.transaction_uncertain


def error_code(error: Exception) -> str:
    if isinstance(error, ServiceError):
        return error.code
    if isinstance(error, TimeoutError):
        return "socket_timeout"
    if isinstance(error, OSError):
        return f"os_error_{error.errno}" if error.errno is not None else "os_error"
    return type(error).__name__


def send_transaction(
    state: dict,
    payload: dict,
    accepted: dict,
    *,
    request,
    save_state,
    replace_state,
    force_synced: bool = True,
    advance_sequence: bool = True,
) -> None:
    """Persist a write-ahead state before a sequenced service write."""
    previous = copy.deepcopy(state)
    staged = copy.deepcopy(state)
    staged["next_seq"] = state["next_seq"] + 1
    staged["transaction_uncertain"] = True
    save_state(staged)
    replace_state(state, staged)
    try:
        request(payload)
    except (OSError, ServiceError) as error:
        if not is_uncertain(error):
            try:
                save_state(previous)
                replace_state(state, previous)
            except (OSError, ValueError) as rollback_error:
                raise ServiceError("state_persistence_uncertain", transaction_uncertain=True) from rollback_error
        raise

    finalized = copy.deepcopy(accepted)
    if advance_sequence:
        finalized["next_seq"] = staged["next_seq"]
    if force_synced:
        finalized["needs_sync"] = False
    finalized["transaction_uncertain"] = False
    try:
        save_state(finalized)
    except (OSError, ValueError) as error:
        raise ServiceError("state_persistence_uncertain", transaction_uncertain=True) from error
    replace_state(state, finalized)


def validate_sequence(value: object) -> bool:
    return type(value) is int and 1 <= value <= MAX_SAFE_INTEGER
