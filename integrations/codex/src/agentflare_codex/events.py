"""Allowlisted lifecycle event translation, identities, and snapshots."""

import hashlib
from typing import Optional
import unicodedata

from .config import COMPACT_DIGEST_HEX, MAX_IDENTIFIER_BYTES, MAX_SAFE_INTEGER


DEFAULT_SESSION_ID = "codex-hook-session"
SOURCE = "codex-hook"
MAIN_AGENT_ID = "codex-main"

ACTIVE_STATES = frozenset({"running", "waiting_user"})
TERMINAL_STATES = frozenset({"succeeded", "failed", "cancelled"})

VALUE_FIELDS = (
    "hook_event_name", "session_id", "turn_id", "agent_id", "agent_type",
    "permission_mode", "stop_hook_active", "source", "reason", "tool_name",
)
PRESENCE_FIELDS = (
    "prompt", "tool_input", "last_assistant_message", "agent_transcript_path",
    "transcript_path",
)
DISPLAY_STATES = {
    "UserPromptSubmit": ("running", None),
    "PermissionRequest": ("waiting_user", "approval"),
    "PreToolUse": ("running", None),
    "PostToolUse": ("running", None),
    "Stop": ("succeeded", None),
    "Interrupt": ("cancelled", "user_cancelled"),
}
REQUEST_USER_INPUT_TOOL = "request_user_input"


def display_state_for(payload: dict) -> Optional[tuple[str, Optional[str]]]:
    """Map one allowlisted Codex hook to a semantic display state."""
    event_name = payload.get("hook_event_name")
    tool_name = payload.get("tool_name")
    if not isinstance(event_name, str):
        return None
    if event_name == "PreToolUse" and tool_name == REQUEST_USER_INPUT_TOOL:
        return "waiting_user", "decision"
    if event_name == "PostToolUse" and tool_name == REQUEST_USER_INPUT_TOOL:
        return "running", "decision"
    return DISPLAY_STATES.get(event_name)


def text_bytes(value: str) -> bytes:
    return value.encode("utf-8", "surrogatepass")


def framed_digest(*values: str) -> str:
    digest = hashlib.sha256()
    for value in values:
        encoded = text_bytes(value)
        digest.update(len(encoded).to_bytes(8, "big"))
        digest.update(encoded)
    return digest.hexdigest()


def compact_identifier(prefix: str, *values: str) -> str:
    return f"{prefix}{framed_digest(*values)[:COMPACT_DIGEST_HEX]}"


def compact_session_id(value: str) -> str:
    return compact_identifier("cs-", "session", value)


def compact_agent_id(session: str, agent: str) -> str:
    return compact_identifier("sa-", "agent", session, agent)


def compact_run_id(session: str, agent: str, turn: str, start_order: int) -> str:
    return compact_identifier("sr-", "run", session, agent, turn, str(start_order))


def compact_main_run_id(session: str, turn: str) -> str:
    return compact_identifier("mr-", "main", session, turn)


def safe_identifier(value: object, fallback: str) -> str:
    if not isinstance(value, str) or not value.strip():
        return fallback
    normalized = "".join(
        character if character.isprintable() and character not in "\r\n\t" else "_"
        for character in value
    )
    while normalized and len(text_bytes(normalized)) > MAX_IDENTIFIER_BYTES:
        normalized = normalized[:-1]
    return normalized or fallback


def valid_identifier(value: object, *, allow_empty: bool = False) -> bool:
    if not isinstance(value, str) or (not allow_empty and not value):
        return False
    try:
        encoded = value.encode("utf-8")
    except UnicodeEncodeError:
        return False
    return len(encoded) <= MAX_IDENTIFIER_BYTES and all(
        unicodedata.category(character) != "Cc" for character in value
    )


def valid_local_string(value: object) -> bool:
    return isinstance(value, str) and bool(value.strip())


def session_id(payload: dict) -> str:
    value = payload.get("session_id")
    return value if valid_local_string(value) else DEFAULT_SESSION_ID


def required_subagent_fields(payload: dict) -> tuple[str, str, str, str]:
    if not isinstance(payload, dict):
        raise ValueError("subagent hook payload must be an object")
    values = []
    for field in ("session_id", "turn_id", "agent_id", "agent_type"):
        value = payload.get(field)
        if not valid_local_string(value):
            raise ValueError(f"{field} is required for subagent hooks")
        values.append(value)
    return values[0], values[1], values[2], values[3]


def local_agent_key(agent: str) -> str:
    return compact_identifier("la-", "local-agent", agent)


def run_id(payload: dict) -> str:
    turn = payload.get("turn_id")
    if not isinstance(turn, str) or not turn:
        turn = "implicit-turn"
    return compact_main_run_id(session_id(payload), turn)


def main_turn_id(payload: dict) -> str:
    turn = payload.get("turn_id")
    if not isinstance(turn, str) or not turn:
        turn = "implicit-turn"
    return compact_identifier("mt-", "turn", turn)


def main_run_for_event(state: dict, payload: dict, display_state: str) -> tuple[str, str, int]:
    """Choose a main run identity without reopening a terminal run."""
    turn = main_turn_id(payload)
    current = state.get("main")
    if isinstance(current, dict) and current.get("turn_id") == turn:
        if display_state in TERMINAL_STATES or current.get("state") in ACTIVE_STATES:
            return current["run_id"], turn, current["run_generation"]
        generation = current["run_generation"] + 1
        if generation > MAX_SAFE_INTEGER:
            raise ValueError("main run generation exhausted")
        return compact_main_run_id(
            session_id(payload), f"resume:{turn}:{generation}"
        ), turn, generation
    return run_id(payload), turn, 0


def main_snapshot(state: dict) -> Optional[dict]:
    """Return the active main agent of a desired state, or None."""
    main = state.get("main")
    if not isinstance(main, dict) or main.get("state") not in ACTIVE_STATES:
        return None
    return {
        "agent_id": MAIN_AGENT_ID,
        "run_id": main["run_id"],
        "state": main["state"],
    }


def subagent_snapshots(state: dict) -> list[dict]:
    """Return active subagents of a desired state in stable display order."""
    values = []
    for record in state.get("subagents", {}).values():
        if record.get("state") not in ACTIVE_STATES:
            continue
        values.append(
            {
                "agent_id": record["agent_id"],
                "run_id": record["run_id"],
                "parent_agent_id": record["parent_agent_id"],
                "state": record["state"],
                "start_order": record["start_order"],
            }
        )
    values.sort(
        key=lambda item: (item["start_order"], item["agent_id"], item["run_id"])
    )
    return values


def sync_request(state: dict, snapshot: Optional[dict] = None) -> dict:
    """Build a complete display snapshot without performing I/O."""
    source = snapshot if snapshot is not None else state
    return {
        "version": 2,
        "type": "display_sync",
        "binding_id": state["binding_id"],
        "seq": state["next_seq"],
        "main": main_snapshot(source),
        "subagents": subagent_snapshots(source),
    }
