#!/usr/bin/env python3

import contextlib
import copy
import io
import json
import os
import subprocess
import tempfile
import threading
import time
import unittest
from pathlib import Path
import sys


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))
from agentflare_codex import cli, events, protocol, session as bridge, storage
from agentflare_codex.config import MAX_SUBAGENTS


BRIDGE_PATH = ROOT / "agentflare_bridge.py"


HOOKS_PATH = ROOT / "hooks.json"


class FakeService:
    def __init__(self):
        self.requests = []
        self.fail_next_event = None
        self.fail_next_sync = None
        self.bound_session = None
        self.binding_counter = 0
        self.binding_id = None
        self.next_seq = 0
        self.main = None
        self.subagents = {}

    def request(self, payload):
        payload = copy.deepcopy(payload)
        self.requests.append(payload)
        request_type = payload["type"]
        if request_type == "status":
            if self.bound_session is None:
                return {}
            return {"source": bridge.SOURCE, "session_id": self.bound_session}
        if request_type == "display_bind":
            self.bound_session = payload["session_id"]
            self.binding_counter += 1
            self.binding_id = f"binding-{self.binding_counter}"
            self.next_seq = 1
            return {
                "binding_id": self.binding_id,
                "next_seq": 1,
                "lease_ms": 15000,
            }
        if request_type == "display_event":
            if self.fail_next_event is not None:
                failure = self.fail_next_event
                self.fail_next_event = None
                raise bridge.ServiceError(
                    failure, transaction_uncertain=failure == "service_no_response"
                )
            self._validate_write(payload)
            if payload["role"] == "main":
                if payload["state"] in bridge.TERMINAL_STATES and self.main is None:
                    raise bridge.ServiceError("unknown_agent")
                self.main = payload["state"]
            else:
                key = (payload["agent_id"], payload["run_id"])
                self.subagents[key] = payload["state"]
            return {"accepted_seq": payload["seq"]}
        if request_type == "display_sync":
            if self.fail_next_sync is not None:
                failure = self.fail_next_sync
                self.fail_next_sync = None
                raise bridge.ServiceError(failure)
            self._validate_write(payload)
            main = payload["main"]
            self.main = main["state"] if main is not None else None
            self.subagents = {
                (item["agent_id"], item["run_id"]): item["state"]
                for item in payload["subagents"]
            }
            return {"accepted_seq": payload["seq"]}
        if request_type == "display_unbind":
            self._validate_write(payload)
            self.bound_session = None
            self.binding_id = None
            self.main = None
            self.subagents = {}
            return {"accepted_seq": payload["seq"], "unbound": True}
        self._validate_write(payload)
        return {"accepted_seq": payload["seq"]}

    def _validate_write(self, payload):
        if payload["binding_id"] != self.binding_id:
            raise bridge.ServiceError("unknown_binding")
        if payload["seq"] != self.next_seq:
            raise AssertionError(
                f"expected sequence {self.next_seq}, got {payload['seq']}"
            )
        self.next_seq += 1


class BridgeTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        root = Path(self.directory.name)
        storage.STATE_DIR = str(root / "state")
        storage.SOURCE_FRESHNESS_GRACE = 90.0
        cli.OUTPUT_PATH = str(root / "events.jsonl")
        self.service = FakeService()
        self.original_request = bridge.request
        self.original_start_worker = bridge.start_worker
        self.original_start_worker_locked = bridge._start_worker_locked
        bridge.request = self.service.request
        bridge.start_worker = lambda _session: None
        bridge._start_worker_locked = lambda _session, _state: None

    def tearDown(self):
        bridge.request = self.original_request
        bridge.start_worker = self.original_start_worker
        bridge._start_worker_locked = self.original_start_worker_locked
        self.directory.cleanup()

    def payload(self, event_name="UserPromptSubmit", session="session-a", turn="turn-a"):
        return {
            "hook_event_name": event_name,
            "session_id": session,
            "turn_id": turn,
        }

    def subagent_payload(self, event_name="SubagentStart", agent="worker-a", turn="turn-a"):
        payload = self.payload(event_name, turn=turn)
        payload.update({"agent_id": agent, "agent_type": "worker"})
        return payload

    def test_new_session_binds_syncs_and_delivers(self):
        bridge.send_display_state(self.payload(), "running", None)
        self.assertEqual(
            [request["type"] for request in self.service.requests],
            ["status", "display_bind", "display_sync", "display_event"],
        )
        bind = self.service.requests[1]
        self.assertEqual(bind["session_id"], events.compact_session_id("session-a"))
        state = storage.load_state("session-a")
        self.assertIsNotNone(state)
        assert state is not None
        self.assertEqual(state["next_seq"], 3)
        self.assertFalse(state["needs_sync"])
        self.assertEqual(self.service.requests[2]["main"], None)

    def test_heartbeat_consumes_sequence_and_is_accepted(self):
        bridge.send_display_state(self.payload(), "running", None)
        self.service.requests.clear()
        bridge.send_heartbeat("session-a")
        self.assertEqual(self.service.requests[0]["type"], "display_heartbeat")
        self.assertEqual(self.service.requests[0]["seq"], 3)
        state = storage.load_state("session-a")
        assert state is not None
        self.assertEqual(state["next_seq"], 4)

    def test_unknown_binding_rebinds_with_snapshot_and_retries_main(self):
        bridge.send_display_state(self.payload(), "running", None)
        self.service.requests.clear()
        self.service.fail_next_event = "unknown_binding"
        bridge.send_display_state(self.payload("Stop"), "succeeded", None)
        self.assertEqual(
            [request["type"] for request in self.service.requests],
            ["display_event", "display_bind", "display_sync", "display_event"],
        )
        self.assertEqual(self.service.requests[-2]["main"]["state"], "running")
        self.assertEqual(self.service.requests[-1]["state"], "succeeded")

    def test_terminal_without_start_bootstraps_only_empty_main(self):
        self.service.fail_next_event = "unknown_agent"
        bridge.send_display_state(self.payload("Stop"), "succeeded", None)
        self.assertEqual(
            [request["type"] for request in self.service.requests],
            [
                "status",
                "display_bind",
                "display_sync",
                "display_event",
                "display_event",
                "display_event",
            ],
        )
        self.assertEqual(self.service.requests[-2]["state"], "running")
        self.assertEqual(self.service.requests[-1]["state"], "succeeded")

    def test_unknown_agent_does_not_replace_known_main(self):
        bridge.send_display_state(self.payload(), "running", None)
        self.service.requests.clear()
        self.service.fail_next_event = "unknown_agent"
        with self.assertRaises(bridge.ServiceError) as context:
            bridge.send_display_state(
                self.payload("Stop", turn="turn-other"), "succeeded", None
            )
        self.assertEqual(str(context.exception), "unknown_agent")
        self.assertEqual(len(self.service.requests), 1)

    def test_uncertain_transaction_blocks_follow_up_requests(self):
        bridge.send_display_state(self.payload(), "running", None)
        self.service.requests.clear()
        self.service.fail_next_event = "service_no_response"
        with self.assertRaises(bridge.ServiceError) as context:
            bridge.send_display_state(self.payload("Stop"), "succeeded", None)
        self.assertEqual(str(context.exception), "service_no_response")
        before = len(self.service.requests)
        with self.assertRaises(bridge.ServiceError) as context:
            bridge.send_display_state(self.payload(), "running", None)
        self.assertEqual(str(context.exception), "service_transaction_uncertain")
        self.assertEqual(len(self.service.requests), before)

    def test_uncertain_initial_bind_is_persisted_and_blocks_follow_up(self):
        def fail_bind(payload):
            if payload["type"] == "display_bind":
                raise bridge.ServiceError(
                    "service_no_response", transaction_uncertain=True
                )
            return self.service.request(payload)

        bridge.request = fail_bind
        with self.assertRaises(bridge.ServiceError):
            bridge.send_display_state(self.payload(), "running", None)
        state = storage.load_state("session-a")
        self.assertIsNotNone(state)
        assert state is not None
        self.assertTrue(state["transaction_uncertain"])
        bridge.request = self.service.request
        with self.assertRaises(bridge.ServiceError) as context:
            bridge.send_display_state(self.payload(), "running", None)
        self.assertEqual(str(context.exception), "service_transaction_uncertain")

    def test_uncertain_session_has_explicit_fresh_binding_recovery(self):
        bridge.send_display_state(self.payload(), "running", None)
        self.service.fail_next_event = "service_no_response"
        with self.assertRaises(bridge.ServiceError):
            bridge.send_display_state(self.payload("Stop"), "succeeded", None)
        cli.handle_payload(self.payload("SessionEnd"))
        cli.handle_payload(self.payload("SessionStart"))
        with self.assertRaisesRegex(bridge.ServiceError, "service_transaction_uncertain"):
            bridge.send_display_state(self.payload(), "running", None)

        recovered = bridge.recover_uncertain_session("session-a")
        self.assertEqual(recovered["next_seq"], 2)
        self.assertFalse(recovered["transaction_uncertain"])
        self.assertEqual(self.service.main, None)
        bridge.send_display_state(self.payload("UserPromptSubmit", turn="turn-new"), "running", None)
        self.assertEqual(self.service.main, "running")
        self.assertGreaterEqual(self.service.binding_counter, 2)

    def test_recovery_without_uncertainty_does_not_change_active_session(self):
        bridge.send_display_state(self.payload(session="session-a"), "running", None)
        self.service.requests.clear()
        with self.assertRaisesRegex(bridge.ServiceError, "service_recovery_not_required"):
            bridge.recover_uncertain_session("session-b")
        self.assertEqual(self.service.requests, [])
        self.assertEqual(
            self.service.bound_session, events.compact_session_id("session-a")
        )

    def test_state_paths_are_isolated_by_session(self):
        self.assertNotEqual(storage.state_path("session-a"), storage.state_path("session-b"))
        bridge.send_display_state(self.payload(session="session-a"), "running", None)
        bridge.send_display_state(self.payload(session="session-b"), "running", None)
        self.assertEqual(
            len(
                [
                    path
                    for path in Path(storage.STATE_DIR).glob("*.json")
                    if path.name != "active-session.json"
                ]
            ),
            2,
        )

    def test_newly_active_session_releases_previous_binding(self):
        bridge.send_display_state(self.payload(session="session-a"), "running", None)
        self.service.requests.clear()

        bridge.send_display_state(self.payload(session="session-b"), "running", None)

        self.assertEqual(
            [request["type"] for request in self.service.requests],
            ["status", "display_unbind", "display_bind", "display_sync", "display_event"],
        )
        self.assertEqual(
            self.service.bound_session, events.compact_session_id("session-b")
        )

    def test_session_start_does_not_claim_the_display(self):
        cli.handle_payload(self.payload("SessionStart", session="session-a"))
        self.assertEqual(self.service.requests, [])
        self.assertIsNone(storage.load_active_session())

    def test_structured_user_input_turns_blue_then_running(self):
        pre_tool = self.payload("PreToolUse")
        pre_tool["tool_name"] = "request_user_input"
        self.assertEqual(
            events.display_state_for(pre_tool),
            ("waiting_user", "decision"),
        )
        cli.handle_payload(pre_tool)
        self.assertEqual(self.service.requests[-1]["state"], "waiting_user")

        post_tool = self.payload("PostToolUse")
        post_tool["tool_name"] = "request_user_input"
        self.assertEqual(
            events.display_state_for(post_tool),
            ("running", "decision"),
        )
        cli.handle_payload(post_tool)
        self.assertEqual(self.service.requests[-1]["state"], "running")

    def test_subagent_start_sends_running_event_and_persists_snapshot(self):
        result = bridge.send_subagent_start(self.subagent_payload())
        self.assertEqual(result, "accepted")
        event = self.service.requests[-1]
        self.assertEqual(event["type"], "display_event")
        self.assertEqual(event["role"], "subagent")
        self.assertEqual(event["state"], "running")
        self.assertEqual(event["parent_agent_id"], bridge.MAIN_AGENT_ID)
        self.assertLessEqual(len(event["agent_id"].encode()), 128)
        self.assertLessEqual(len(event["run_id"].encode()), 128)
        state = storage.load_state("session-a")
        assert state is not None
        self.assertEqual(len(state["subagents"]), 1)
        self.assertEqual(state["subagents"][next(iter(state["subagents"]))]["start_order"], 1)

    def test_duplicate_start_is_idempotent(self):
        payload = self.subagent_payload()
        bridge.send_subagent_start(payload)
        before = len(self.service.requests)
        self.assertEqual(bridge.send_subagent_start(payload), "duplicate")
        self.assertEqual(len(self.service.requests), before)

    def test_subagent_stop_sends_full_snapshot_without_terminal_state(self):
        payload = self.subagent_payload()
        bridge.send_subagent_start(payload)
        self.service.requests.clear()
        stop = self.subagent_payload("SubagentStop")
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            self.assertEqual(cli.handle_payload(stop), 0)
        self.assertEqual(output.getvalue(), '{"continue":true}\n')
        self.assertEqual([request["type"] for request in self.service.requests], ["display_sync"])
        self.assertEqual(self.service.requests[0]["subagents"], [])
        self.assertIsNone(self.service.requests[0]["main"])
        self.assertNotIn("state", self.service.requests[0])
        state = storage.load_state("session-a")
        assert state is not None
        self.assertEqual(state["subagents"], {})

    def test_repeated_stop_is_unknown_without_service_event(self):
        payload = self.subagent_payload()
        bridge.send_subagent_start(payload)
        bridge.send_subagent_stop(self.subagent_payload("SubagentStop"))
        before = len(self.service.requests)
        self.assertEqual(bridge.send_subagent_stop(self.subagent_payload("SubagentStop")), "unknown")
        self.assertEqual(len(self.service.requests), before)

    def test_continuing_stop_hook_is_non_blocking_and_stays_unobserved(self):
        payload = self.subagent_payload()
        bridge.send_subagent_start(payload)
        stop = self.subagent_payload("SubagentStop")
        stop["stop_hook_active"] = True
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            cli.handle_payload(stop)
        self.assertEqual(output.getvalue(), '{"continue":true}\n')
        before = len(self.service.requests)
        with contextlib.redirect_stdout(output):
            cli.handle_payload(stop)
        self.assertEqual(len(self.service.requests), before)
        events = [
            json.loads(line)
            for line in Path(cli.OUTPUT_PATH).read_text(encoding="utf-8").splitlines()
        ]
        self.assertTrue(any(event.get("bridge_event") == "subagent_stop_unknown" for event in events))

    def test_agent_id_reuse_gets_new_run_and_order(self):
        payload = self.subagent_payload()
        bridge.send_subagent_start(payload)
        first = storage.load_state("session-a")
        assert first is not None
        first_record = copy.deepcopy(next(iter(first["subagents"].values())))
        bridge.send_subagent_stop(self.subagent_payload("SubagentStop"))
        bridge.send_subagent_start(payload)
        second = storage.load_state("session-a")
        assert second is not None
        second_record = next(iter(second["subagents"].values()))
        self.assertNotEqual(first_record["run_id"], second_record["run_id"])
        self.assertGreater(second_record["start_order"], first_record["start_order"])

    def test_late_stop_from_older_turn_keeps_newer_run(self):
        bridge.send_subagent_start(self.subagent_payload(agent="worker-a", turn="turn-old"))
        bridge.send_subagent_stop(self.subagent_payload("SubagentStop", agent="worker-a", turn="turn-old"))
        bridge.send_subagent_start(self.subagent_payload(agent="worker-a", turn="turn-new"))
        before = storage.load_state("session-a")
        assert before is not None
        expected_record = copy.deepcopy(next(iter(before["subagents"].values())))
        self.service.requests.clear()
        self.assertEqual(
            bridge.send_subagent_stop(
                self.subagent_payload("SubagentStop", agent="worker-a", turn="turn-old")
            ),
            "unknown",
        )
        self.assertEqual(self.service.requests, [])
        after = storage.load_state("session-a")
        assert after is not None
        self.assertEqual(after["next_seq"], before["next_seq"])
        self.assertEqual(after["subagents"], before["subagents"])
        self.assertEqual(next(iter(after["subagents"].values())), expected_record)
        self.assertEqual(
            bridge.send_subagent_stop(
                self.subagent_payload("SubagentStop", agent="worker-a", turn="turn-new")
            ),
            "accepted",
        )
        stopped = storage.load_state("session-a")
        assert stopped is not None
        self.assertEqual(stopped["subagents"], {})
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            self.assertEqual(
                cli.handle_payload(
                    self.subagent_payload("SubagentStop", agent="worker-a", turn="turn-old")
                ),
                0,
            )
        self.assertEqual(output.getvalue(), '{"continue":true}\n')
        logged = [
            json.loads(line)
            for line in Path(cli.OUTPUT_PATH).read_text(encoding="utf-8").splitlines()
        ]
        self.assertTrue(
            any(event.get("bridge_event") == "subagent_stop_unknown" for event in logged)
        )

    def test_late_stop_from_inactive_session_skips_activation(self):
        bridge.send_subagent_start(self.subagent_payload(agent="worker-a", turn="turn-old"))
        bridge.send_subagent_stop(
            self.subagent_payload("SubagentStop", agent="worker-a", turn="turn-old")
        )
        bridge.send_subagent_start(self.subagent_payload(agent="worker-a", turn="turn-new"))
        bridge.send_display_state(
            self.payload(session="session-b", turn="turn-b"), "running", None
        )
        self.assertEqual(storage.load_active_session(), "session-b")
        self.service.requests.clear()
        before_a = copy.deepcopy(storage.load_state("session-a"))
        before_b = copy.deepcopy(storage.load_state("session-b"))
        assert before_a is not None and before_b is not None
        self.assertEqual(
            bridge.send_subagent_stop(
                self.subagent_payload("SubagentStop", agent="worker-a", turn="turn-old")
            ),
            "unknown",
        )
        self.assertEqual(storage.load_active_session(), "session-b")
        self.assertEqual(self.service.requests, [])
        after_a = storage.load_state("session-a")
        after_b = storage.load_state("session-b")
        self.assertEqual(after_a, before_a)
        self.assertEqual(after_b, before_b)
        assert after_a is not None
        self.assertEqual(after_a["next_seq"], before_a["next_seq"])
        self.assertEqual(after_b["next_seq"], before_b["next_seq"])
        record = next(iter(after_a["subagents"].values()))
        self.assertEqual(len(after_a["subagents"]), 1)
        self.assertEqual(
            record["run_id"],
            events.compact_run_id("session-a", "worker-a", "turn-new", record["start_order"]),
        )
        self.assertEqual(
            bridge.send_subagent_stop(
                self.subagent_payload("SubagentStop", agent="worker-a", turn="turn-new")
            ),
            "accepted",
        )
        self.assertEqual(storage.load_active_session(), "session-a")
        final_a = storage.load_state("session-a")
        assert final_a is not None
        self.assertEqual(final_a["subagents"], {})

    def test_subagent_recovery_replays_active_snapshot_before_new_event(self):
        bridge.send_subagent_start(self.subagent_payload(agent="worker-a"))
        self.service.requests.clear()
        self.service.fail_next_event = "unknown_binding"
        bridge.send_subagent_start(self.subagent_payload(agent="worker-b", turn="turn-b"))
        self.assertEqual(
            [request["type"] for request in self.service.requests],
            ["display_event", "display_bind", "display_sync", "display_event"],
        )
        self.assertEqual(len(self.service.requests[2]["subagents"]), 1)
        self.assertEqual(self.service.requests[2]["subagents"][0]["state"], "running")

    def test_capacity_is_bounded_without_eviction(self):
        for index in range(MAX_SUBAGENTS):
            bridge.send_subagent_start(
                self.subagent_payload(agent=f"worker-{index}", turn=f"turn-{index}")
            )
        before = len(self.service.requests)
        with self.assertRaises(bridge.ServiceError) as context:
            bridge.send_subagent_start(self.subagent_payload(agent="worker-overflow"))
        self.assertEqual(str(context.exception), "capacity_exceeded")
        self.assertEqual(len(self.service.requests), before)
        state = storage.load_state("session-a")
        assert state is not None
        self.assertEqual(len(state["subagents"]), MAX_SUBAGENTS)

    def test_service_capacity_error_does_not_consume_sequence_or_state(self):
        bridge.send_subagent_start(self.subagent_payload(agent="worker-a"))
        before = storage.load_state("session-a")
        assert before is not None
        self.service.fail_next_event = "capacity_exceeded"
        with self.assertRaises(bridge.ServiceError) as context:
            bridge.send_subagent_start(self.subagent_payload(agent="worker-b"))
        self.assertEqual(str(context.exception), "capacity_exceeded")
        after = storage.load_state("session-a")
        assert after is not None
        self.assertEqual(after["next_seq"], before["next_seq"])
        self.assertEqual(after["subagents"], before["subagents"])

    def test_compact_ids_handle_unicode_and_controls(self):
        session = " Sitzung\n" + "界" * 400
        agent = "worker\x00\t" + "é" * 400
        agent_id = events.compact_agent_id(session, agent)
        run = events.compact_run_id(session, agent, "turn\n", 1)
        self.assertLessEqual(len(events.compact_session_id(session).encode()), 128)
        self.assertLessEqual(len(agent_id.encode()), 128)
        self.assertLessEqual(len(run.encode()), 128)
        self.assertTrue(all(ord(character) >= 32 for character in agent_id))
        self.assertNotEqual(agent_id, events.compact_agent_id(session, agent + "x"))

    def test_maximum_snapshot_is_smaller_than_socket_limit(self):
        state = storage.initial_state("session-a")
        state["binding_id"] = "binding-1"
        state["next_seq"] = 1
        for index in range(MAX_SUBAGENTS):
            state["subagents"][events.local_agent_key(f"agent-{index}")] = bridge._subagent_record(
                "session-a", f"turn-{index}", f"agent-{index}", index + 1
            )
        encoded = protocol.encode_request(events.sync_request(state))
        self.assertLess(len(encoded), protocol.MAX_REQUEST_BYTES)

    def test_oversized_request_is_rejected_before_socket_connect(self):
        original_socket = bridge.protocol.socket.socket

        class FailingSocket:
            def __call__(self, *_args, **_kwargs):
                raise AssertionError("socket must not be opened")

        bridge.protocol.socket.socket = FailingSocket()
        try:
            with self.assertRaises(bridge.ServiceError) as context:
                self.original_request(
                    {"version": 2, "type": "status", "padding": "x" * 20000}
                )
            self.assertEqual(str(context.exception), "request_too_large")
        finally:
            bridge.protocol.socket.socket = original_socket

    def test_mismatched_accepted_sequence_is_uncertain(self):
        original_socket = bridge.protocol.socket.socket

        class ReplyingSocket:
            def __enter__(self):
                return self

            def __exit__(self, *_args):
                return False

            def settimeout(self, _timeout):
                pass

            def connect(self, _path):
                pass

            def sendall(self, _payload):
                pass

            def recv(self, _size):
                return b'{"ok":true,"result":{"accepted_seq":8}}\n'

        bridge.protocol.socket.socket = lambda *_args, **_kwargs: ReplyingSocket()
        try:
            with self.assertRaises(bridge.ServiceError) as context:
                self.original_request(
                    {
                        "version": 2,
                        "type": "display_heartbeat",
                        "binding_id": "binding-1",
                        "seq": 7,
                    }
                )
            self.assertEqual(str(context.exception), "service_invalid_accepted_seq")
            self.assertTrue(context.exception.transaction_uncertain)
        finally:
            bridge.protocol.socket.socket = original_socket

    def request_over_socket(self, payload, chunks):
        original_socket = bridge.protocol.socket.socket

        class ChunkedSocket:
            def __init__(self):
                self._chunks = list(chunks)

            def __enter__(self):
                return self

            def __exit__(self, *_args):
                return False

            def settimeout(self, _timeout):
                pass

            def connect(self, _path):
                pass

            def sendall(self, _payload):
                pass

            def recv(self, _size):
                if self._chunks:
                    return self._chunks.pop(0)
                return b""

        bridge.protocol.socket.socket = lambda *_args, **_kwargs: ChunkedSocket()
        try:
            return self.original_request(payload)
        finally:
            bridge.protocol.socket.socket = original_socket

    @staticmethod
    def chunked(wire, size=4096):
        return [wire[index : index + size] for index in range(0, len(wire), size)]

    def heartbeat_payload(self, seq=7):
        return {
            "version": 2,
            "type": "display_heartbeat",
            "binding_id": "binding-1",
            "seq": seq,
        }

    def test_response_above_request_limit_is_read_completely(self):
        wire = (
            json.dumps({"ok": True, "result": {"accepted_seq": 7, "padding": "y" * 20000}})
            + "\n"
        ).encode()
        self.assertGreater(len(wire), protocol.MAX_REQUEST_BYTES)
        self.assertLessEqual(len(wire), protocol.MAX_RESPONSE_BYTES)
        response = self.request_over_socket(
            self.heartbeat_payload(), self.chunked(wire)
        )
        self.assertEqual(response["accepted_seq"], 7)

    def test_small_response_in_many_chunks_is_assembled(self):
        wire = (
            json.dumps({"ok": True, "result": {"accepted_seq": 7}}) + "\n"
        ).encode()
        response = self.request_over_socket(
            self.heartbeat_payload(), self.chunked(wire, size=7)
        )
        self.assertEqual(response["accepted_seq"], 7)

    def test_response_at_response_limit_is_accepted(self):
        envelope = {"ok": True, "result": {"accepted_seq": 7, "padding": ""}}
        need = protocol.MAX_RESPONSE_BYTES - len((json.dumps(envelope) + "\n").encode())
        envelope["result"]["padding"] = "y" * need
        wire = (json.dumps(envelope) + "\n").encode()
        self.assertEqual(len(wire), protocol.MAX_RESPONSE_BYTES)
        response = self.request_over_socket(
            self.heartbeat_payload(), self.chunked(wire)
        )
        self.assertEqual(response["accepted_seq"], 7)

    def test_response_above_response_limit_is_rejected_uncertain(self):
        wire = b"x" * (protocol.MAX_RESPONSE_BYTES + 4096)
        with self.assertRaises(bridge.ServiceError) as context:
            self.request_over_socket(self.heartbeat_payload(), self.chunked(wire))
        self.assertEqual(str(context.exception), "service_response_too_large")
        self.assertTrue(context.exception.transaction_uncertain)

    def test_full_status_with_64_subagents_is_readable(self):
        queue = [
            {
                "agent_id": f"worker-{index:02d}-" + "w" * 180,
                "run_id": f"run-{index:02d}-" + "r" * 180,
                "state": "running",
            }
            for index in range(MAX_SUBAGENTS)
        ]
        wire = (
            json.dumps({"ok": True, "result": {"queue": queue, "queue_length": 64}})
            + "\n"
        ).encode()
        self.assertGreater(len(wire), protocol.MAX_REQUEST_BYTES)
        self.assertLessEqual(len(wire), protocol.MAX_RESPONSE_BYTES)
        response = self.request_over_socket(
            {"version": 2, "type": "status"}, self.chunked(wire)
        )
        self.assertEqual(len(response["queue"]), MAX_SUBAGENTS)
        self.assertEqual(response["queue"][0]["state"], "running")
        self.assertEqual(response["queue"][-1]["run_id"], queue[-1]["run_id"])

    def test_rebind_replays_active_main_and_all_subagents(self):
        bridge.send_display_state(self.payload(), "running", None)
        bridge.send_subagent_start(self.subagent_payload(agent="worker-a"))
        bridge.send_subagent_start(
            self.subagent_payload(agent="worker-b", turn="turn-b")
        )
        state = storage.load_state("session-a")
        assert state is not None
        state["binding_id"] = ""
        state["next_seq"] = 0
        state["needs_sync"] = True
        storage.save_state(state)
        self.service.requests.clear()
        bridge.send_subagent_start(self.subagent_payload(agent="worker-c", turn="turn-c"))
        sync = next(
            request for request in self.service.requests if request["type"] == "display_sync"
        )
        self.assertEqual(sync["main"]["state"], "running")
        self.assertEqual(len(sync["subagents"]), 2)

    def test_parallel_main_start_and_stop_keep_sequence_unique(self):
        bridge.send_subagent_start(
            self.subagent_payload(agent="worker-stop", turn="turn-worker-stop")
        )
        self.service.requests.clear()
        barrier = threading.Barrier(4)
        errors = []

        def send_main():
            try:
                barrier.wait()
                bridge.send_display_state(self.payload(), "running", None)
            except Exception as error:  # pragma: no cover - assertion below reports it
                errors.append(error)

        def start_subagent():
            try:
                barrier.wait()
                bridge.send_subagent_start(
                    self.subagent_payload(agent="worker-start", turn="turn-worker-start")
                )
            except Exception as error:  # pragma: no cover - assertion below reports it
                errors.append(error)

        def stop_subagent():
            try:
                barrier.wait()
                bridge.send_subagent_stop(
                    self.subagent_payload(
                        "SubagentStop",
                        agent="worker-stop",
                        turn="turn-worker-stop",
                    )
                )
            except Exception as error:  # pragma: no cover - assertion below reports it
                errors.append(error)

        workers = [
            threading.Thread(target=send_main),
            threading.Thread(target=start_subagent),
            threading.Thread(target=stop_subagent),
        ]
        for worker in workers:
            worker.start()
        barrier.wait()
        for worker in workers:
            worker.join()
        self.assertEqual(errors, [])
        state = storage.load_state("session-a")
        assert state is not None
        self.assertIsNotNone(state["main"])
        self.assertEqual(len(state["subagents"]), 1)
        write_sequences = [
            request["seq"]
            for request in self.service.requests
            if request["type"] in {"display_event", "display_sync"}
        ]
        self.assertEqual(sorted(write_sequences), [3, 4, 5])

    def test_final_persistence_failure_blocks_sequence_reuse(self):
        bridge.send_subagent_start(self.subagent_payload(agent="worker-a"))
        original_save = storage.save_state

        def fail_final_save(state):
            if state["next_seq"] == 4 and not state["transaction_uncertain"]:
                raise OSError("disk full")
            return original_save(state)

        storage.save_state = fail_final_save
        try:
            with self.assertRaises(bridge.ServiceError) as context:
                bridge.send_subagent_start(self.subagent_payload(agent="worker-b"))
            self.assertEqual(str(context.exception), "state_persistence_uncertain")
        finally:
            storage.save_state = original_save

        state = storage.load_state("session-a")
        assert state is not None
        self.assertTrue(state["transaction_uncertain"])
        self.assertEqual(state["next_seq"], 4)
        before = len(self.service.requests)
        with self.assertRaises(bridge.ServiceError) as context:
            bridge.send_subagent_start(self.subagent_payload(agent="worker-c"))
        self.assertEqual(str(context.exception), "service_transaction_uncertain")
        self.assertEqual(len(self.service.requests), before)

    def test_state_rejects_nonmonotonic_subagent_order(self):
        bridge.send_subagent_start(self.subagent_payload())
        state = storage.load_state("session-a")
        assert state is not None
        state["next_subagent_start_order"] = 1
        self.assertFalse(storage.valid_state(state, "session-a"))

    def test_active_session_lock_covers_activation_and_delivery(self):
        entered_delivery = threading.Event()
        release_delivery = threading.Event()
        errors = []
        original_delivery = bridge._send_main_with_bootstrap

        def pause_a(state, display_state, payload, reason):
            if payload["session_id"] == "session-a":
                entered_delivery.set()
                self.assertTrue(release_delivery.wait(2))
            return original_delivery(state, display_state, payload, reason)

        bridge._send_main_with_bootstrap = pause_a
        try:
            first = threading.Thread(
                target=lambda: bridge.send_display_state(
                    self.payload(session="session-a"), "running", None
                )
            )
            first.start()
            self.assertTrue(entered_delivery.wait(2))

            def send_b():
                try:
                    bridge.send_display_state(
                        self.payload(session="session-b"), "running", None
                    )
                except Exception as error:  # pragma: no cover - assertion below reports it
                    errors.append(error)

            second = threading.Thread(target=send_b)
            second.start()
            self.assertTrue(second.is_alive())
            release_delivery.set()
            first.join(2)
            second.join(2)
        finally:
            bridge._send_main_with_bootstrap = original_delivery

        self.assertFalse(first.is_alive())
        self.assertFalse(second.is_alive())
        self.assertEqual(errors, [])
        self.assertEqual(storage.load_active_session(), "session-b")
        self.assertEqual(self.service.bound_session, events.compact_session_id("session-b"))

    def test_stop_worker_never_signals_an_unverified_pid(self):
        state = storage.initial_state("session-a")
        state["worker_pid"] = os.getpid()
        state["worker_token"] = "worker-stale-token"
        storage.save_state(state)
        original_matches = bridge.worker_matches
        original_kill = bridge.worker.os.kill
        bridge.worker_matches = lambda _pid, _token: False
        bridge.worker.os.kill = lambda *_args: (_ for _ in ()).throw(
            AssertionError("must not signal an unverified PID")
        )
        try:
            bridge.stop_worker("session-a")
        finally:
            bridge.worker_matches = original_matches
            bridge.worker.os.kill = original_kill
        state = storage.load_state("session-a")
        assert state is not None
        self.assertEqual(state["worker_pid"], 0)
        self.assertEqual(state["worker_token"], "")

    def test_worker_is_reaped_when_ownership_cannot_be_persisted(self):
        storage.save_state(storage.initial_state("session-a"))

        class Worker:
            pid = 4242

            def __init__(self):
                self.terminated = False
                self.waited = False

            def terminate(self):
                self.terminated = True

            def wait(self, timeout):
                self.waited = True
                self.timeout = timeout

        worker = Worker()
        original_popen = bridge.worker.subprocess.Popen
        original_matches = bridge.worker_matches
        original_save = storage.save_state
        bridge.worker.subprocess.Popen = lambda *_args, **_kwargs: worker
        bridge.worker_matches = lambda *_args: False

        def fail_worker_save(state):
            if state["worker_pid"] == worker.pid:
                raise OSError("disk full")
            return original_save(state)

        storage.save_state = fail_worker_save
        try:
            bridge._start_worker_locked = self.original_start_worker_locked
            self.original_start_worker("session-a")
        finally:
            bridge._start_worker_locked = lambda _session, _state: None
            bridge.worker.subprocess.Popen = original_popen
            bridge.worker_matches = original_matches
            storage.save_state = original_save

        self.assertTrue(worker.terminated)
        self.assertTrue(worker.waited)
        state = storage.load_state("session-a")
        assert state is not None
        self.assertEqual(state["worker_pid"], 0)
        self.assertEqual(state["worker_token"], "")

    def test_worker_start_gate_is_removed_only_after_registration(self):
        state = storage.initial_state("session-a")

        class Worker:
            pid = 4242

        original_popen = bridge.worker.subprocess.Popen
        original_matches = bridge.worker_matches
        bridge.worker.subprocess.Popen = lambda *_args, **_kwargs: Worker()
        bridge.worker_matches = lambda *_args: False
        try:
            self.original_start_worker_locked("session-a", state)
        finally:
            bridge.worker.subprocess.Popen = original_popen
            bridge.worker_matches = original_matches
        self.assertFalse(Path(storage.worker_start_path("session-a")).exists())
        persisted = storage.load_state("session-a")
        assert persisted is not None
        self.assertEqual(persisted["worker_pid"], Worker.pid)
        self.assertTrue(persisted["worker_token"].startswith("worker-"))

    def test_worker_start_gate_has_a_bounded_wait(self):
        gate = Path(storage.worker_start_path("session-a"))
        storage.ensure_state_dir()
        gate.touch()
        clock_values = iter((0.0, 0.0, 2.0))
        try:
            self.assertFalse(
                bridge.worker.wait_for_start_registration(
                    str(gate),
                    1.0,
                    threading.Event(),
                    clock=lambda: next(clock_values),
                )
            )
            self.assertTrue(gate.exists())
        finally:
            gate.unlink(missing_ok=True)

    def test_worker_marks_missing_source_as_stale_and_deactivates(self):
        state = storage.initial_state("session-a")
        state["worker_pid"] = os.getpid()
        state["worker_token"] = "worker-test-token"
        storage.save_state(state)
        storage.save_active_session("session-a")
        storage.mark_source_observed("session-a")
        self.assertTrue(storage.source_is_fresh("session-a"))
        os.utime(storage.source_seen_path("session-a"), (time.time() - 2, time.time() - 2))
        original_grace = storage.SOURCE_FRESHNESS_GRACE
        original_signal = bridge.worker.signal.signal
        storage.SOURCE_FRESHNESS_GRACE = 1
        bridge.worker.signal.signal = lambda *_args: None
        try:
            self.assertTrue(bridge.deactivate_stale_session("session-a", "worker-test-token"))
        finally:
            storage.SOURCE_FRESHNESS_GRACE = original_grace
            bridge.worker.signal.signal = original_signal
        self.assertIsNone(storage.load_active_session())
        self.assertFalse(Path(storage.source_seen_path("session-a")).exists())

    def test_fresh_hook_wins_race_with_stale_worker_cleanup(self):
        state = storage.initial_state("session-a")
        state["worker_pid"] = os.getpid()
        state["worker_token"] = "worker-test-token"
        storage.save_state(state)
        storage.save_active_session("session-a")
        storage.mark_source_observed("session-a")
        os.utime(storage.source_seen_path("session-a"), (time.time() - 2, time.time() - 2))
        storage.mark_source_observed("session-a")
        self.assertTrue(bridge.worker_status("session-a", "worker-test-token") is bridge.WorkerStatus.KEEP_RUNNING)
        self.assertTrue(bridge.send_heartbeat("session-a", "worker-test-token"))
        state = storage.load_state("session-a")
        assert state is not None
        self.assertEqual(state["worker_token"], "worker-test-token")
        self.assertTrue(storage.source_is_fresh("session-a"))
        self.assertTrue(any(request["type"] == "display_heartbeat" for request in self.service.requests))

    def test_replaced_worker_cannot_heartbeat_rebind_or_clear_state(self):
        state = storage.initial_state("session-a")
        state["worker_pid"] = os.getpid()
        state["worker_token"] = "worker-current-token"
        storage.save_state(state)
        storage.save_active_session("session-a")
        storage.mark_source_observed("session-a")
        waits = iter([True, False])
        original_wait = bridge.worker.wait_for_next_heartbeat
        original_signal = bridge.worker.signal.signal
        bridge.worker.wait_for_next_heartbeat = lambda *_args: next(waits)
        bridge.worker.signal.signal = lambda *_args: None
        try:
            self.assertEqual(bridge.worker_main("session-a", "worker-old-token"), 0)
        finally:
            bridge.worker.wait_for_next_heartbeat = original_wait
            bridge.worker.signal.signal = original_signal
        self.assertEqual(self.service.requests, [])
        state = storage.load_state("session-a")
        assert state is not None
        self.assertEqual(state["worker_token"], "worker-current-token")
        self.assertTrue(Path(storage.source_seen_path("session-a")).exists())

    def test_uncertain_stale_cleanup_does_not_clear_or_remove_marker(self):
        state = storage.initial_state("session-a")
        state["worker_pid"] = os.getpid()
        state["worker_token"] = "worker-test-token"
        storage.save_state(state)
        storage.save_active_session("session-a")
        storage.mark_source_observed("session-a")
        os.utime(storage.source_seen_path("session-a"), (time.time() - 2, time.time() - 2))
        original_grace = storage.SOURCE_FRESHNESS_GRACE
        original_unbind = bridge._unbind_locked
        original_clear = bridge._clear_desired_state_locked
        cleared = []
        storage.SOURCE_FRESHNESS_GRACE = 1
        bridge._unbind_locked = lambda *_args: (_ for _ in ()).throw(
            bridge.ServiceError("service_transaction_uncertain", transaction_uncertain=True)
        )
        bridge._clear_desired_state_locked = lambda *_args: cleared.append(True)
        try:
            with self.assertRaises(bridge.ServiceError):
                bridge.deactivate_stale_session("session-a", "worker-test-token")
        finally:
            storage.SOURCE_FRESHNESS_GRACE = original_grace
            bridge._unbind_locked = original_unbind
            bridge._clear_desired_state_locked = original_clear
        self.assertEqual(cleared, [])
        self.assertTrue(Path(storage.source_seen_path("session-a")).exists())

    def test_heartbeat_logging_failure_does_not_stop_worker(self):
        waits = iter([True, False])
        heartbeats = []
        state = storage.initial_state("session-a")
        state["worker_pid"] = os.getpid()
        state["worker_token"] = "worker-test-token"
        storage.save_state(state)
        storage.save_active_session("session-a")
        storage.mark_source_observed("session-a")
        original_wait = bridge.worker.wait_for_next_heartbeat
        original_step = bridge._worker_step
        original_record = cli.record
        original_signal = bridge.worker.signal.signal
        bridge.worker.wait_for_next_heartbeat = lambda *_args: next(waits)
        bridge._worker_step = lambda session, _token=None: heartbeats.append(session) or bridge.WorkerStatus.KEEP_RUNNING
        cli.record = lambda *_args, **_kwargs: (_ for _ in ()).throw(
            OSError("disk full")
        )
        bridge.worker.signal.signal = lambda *_args: None
        try:
            with contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(
                    bridge.worker_main(
                        "session-a", "worker-test-token", diagnostic=cli.record_best_effort
                    ), 0
                )
        finally:
            bridge.worker.wait_for_next_heartbeat = original_wait
            bridge._worker_step = original_step
            cli.record = original_record
            bridge.worker.signal.signal = original_signal

        self.assertEqual(heartbeats, ["session-a"])

    def test_accepted_heartbeat_reports_dict_payload_to_diagnostic(self):
        waits = iter([True, False])
        state = storage.initial_state("session-a")
        state["worker_pid"] = os.getpid()
        state["worker_token"] = "worker-test-token"
        storage.save_state(state)
        storage.save_active_session("session-a")
        storage.mark_source_observed("session-a")
        seen = []

        def collect(*args, **kwargs):
            seen.append((args, kwargs))
            return True

        original_wait = bridge.worker.wait_for_next_heartbeat
        original_step = bridge._worker_step
        original_signal = bridge.worker.signal.signal
        bridge.worker.wait_for_next_heartbeat = lambda *_args: next(waits)
        bridge._worker_step = lambda session, _token=None: bridge.WorkerStatus.KEEP_RUNNING
        bridge.worker.signal.signal = lambda *_args: None
        try:
            self.assertEqual(
                bridge.worker_main("session-a", "worker-test-token", diagnostic=collect), 0
            )
        finally:
            bridge.worker.wait_for_next_heartbeat = original_wait
            bridge._worker_step = original_step
            bridge.worker.signal.signal = original_signal
        self.assertEqual(len(seen), 1)
        args, kwargs = seen[0]
        self.assertIsInstance(args[0], dict)
        self.assertEqual(args[0].get("hook_event_name"), "SessionHeartbeat")
        self.assertEqual(args[2], "accepted")
        self.assertEqual(kwargs.get("bridge_event"), "display_heartbeat")

    def test_subagent_stop_stays_valid_when_logging_fails(self):
        original_record = cli.record
        cli.record = lambda *_args, **_kwargs: (_ for _ in ()).throw(OSError("full"))
        output = io.StringIO()
        errors = io.StringIO()
        try:
            with contextlib.redirect_stdout(output), contextlib.redirect_stderr(errors):
                self.assertEqual(
                    cli.handle_payload(self.subagent_payload("SubagentStop")), 0
                )
        finally:
            cli.record = original_record
        self.assertEqual(output.getvalue(), '{"continue":true}\n')

    def test_stop_returns_passive_json_after_successful_delivery(self):
        bridge.send_display_state(self.payload(), "running", None)
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            self.assertEqual(cli.handle_payload(self.payload("Stop")), 0)
        self.assertEqual(output.getvalue(), '{"continue":true}\n')

    def test_stop_is_a_completion_candidate_that_can_be_replaced_by_new_work(self):
        bridge.send_display_state(self.payload(), "running", None)
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            cli.handle_payload(self.payload("Stop", turn="turn-a"))
        self.assertEqual(output.getvalue(), '{"continue":true}\n')
        cli.handle_payload(self.payload("UserPromptSubmit", turn="turn-b"))
        states = [
            request["state"]
            for request in self.service.requests
            if request["type"] == "display_event" and request.get("role") == "main"
        ]
        self.assertEqual(states[-2:], ["succeeded", "running"])
        events = [
            json.loads(line)
            for line in Path(cli.OUTPUT_PATH).read_text(encoding="utf-8").splitlines()
        ]
        self.assertTrue(any(event.get("bridge_event") == "main_completion_candidate" for event in events))

    def test_same_turn_activity_renews_main_run_after_stop(self):
        bridge.send_display_state(self.payload(), "running", None)
        stop = self.payload("Stop", turn="turn-a")
        with contextlib.redirect_stdout(io.StringIO()):
            cli.handle_payload(stop)
        terminal_event = self.service.requests[-1]
        self.assertEqual(terminal_event["state"], "succeeded")

        waiting = self.payload("PermissionRequest", turn="turn-a")
        cli.handle_payload(waiting)
        waiting_event = self.service.requests[-1]
        self.assertEqual(waiting_event["state"], "waiting_user")
        self.assertNotEqual(waiting_event["run_id"], terminal_event["run_id"])

        active = self.payload("PostToolUse", turn="turn-a")
        cli.handle_payload(active)
        active_event = self.service.requests[-1]
        self.assertEqual(active_event["state"], "running")
        self.assertEqual(active_event["run_id"], waiting_event["run_id"])

    def test_stop_returns_passive_json_after_failed_delivery(self):
        bridge.send_display_state(self.payload(), "running", None)
        self.service.fail_next_event = "service_unavailable"
        output = io.StringIO()
        with contextlib.redirect_stdout(output), contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(cli.handle_payload(self.payload("Stop")), 0)
        self.assertEqual(output.getvalue(), '{"continue":true}\n')

    def test_configured_directories_are_not_repermissioned(self):
        original_chmod = bridge.os.chmod
        bridge.os.chmod = lambda *_args: (_ for _ in ()).throw(
            AssertionError("must not chmod an existing configured directory")
        )
        try:
            storage.ensure_state_dir()
            cli.record(self.payload(), None, "accepted")
        finally:
            bridge.os.chmod = original_chmod

    def test_hook_log_appends_entries(self):
        cli.record(self.payload(), "running", "accepted")
        cli.record(self.payload(turn="turn-b"), "running", "accepted")
        lines = Path(cli.OUTPUT_PATH).read_text(encoding="utf-8").splitlines()
        self.assertEqual(len(lines), 2)
        entries = [json.loads(line) for line in lines]
        self.assertEqual(entries[0]["turn_id"], "turn-a")
        self.assertEqual(entries[1]["turn_id"], "turn-b")

    def test_hook_log_truncates_when_limit_crossed(self):
        original_limit = cli.MAX_LOG_BYTES
        cli.MAX_LOG_BYTES = 300
        try:
            cli.record(self.payload(), "running", "accepted")
            first_size = os.path.getsize(cli.OUTPUT_PATH)
            self.assertGreater(first_size, 150)
            cli.record(self.payload(turn="turn-b"), "running", "accepted")
            cli.record(self.payload(turn="turn-c"), "running", "accepted")
        finally:
            cli.MAX_LOG_BYTES = original_limit
        self.assertLessEqual(os.path.getsize(cli.OUTPUT_PATH), 300)
        lines = Path(cli.OUTPUT_PATH).read_text(encoding="utf-8").splitlines()
        self.assertEqual(len(lines), 1)
        self.assertEqual(json.loads(lines[0])["turn_id"], "turn-c")

    def test_hook_log_repairs_oversized_file(self):
        original_limit = cli.MAX_LOG_BYTES
        cli.MAX_LOG_BYTES = 300
        try:
            Path(cli.OUTPUT_PATH).write_bytes(b"x" * 500)
            cli.record(self.payload(), "running", "accepted")
        finally:
            cli.MAX_LOG_BYTES = original_limit
        lines = Path(cli.OUTPUT_PATH).read_text(encoding="utf-8").splitlines()
        self.assertEqual(len(lines), 1)
        self.assertEqual(json.loads(lines[0])["display_delivery"], "accepted")

    def test_hook_log_rejects_single_oversized_entry(self):
        original_limit = cli.MAX_LOG_BYTES
        cli.MAX_LOG_BYTES = 300
        try:
            cli.record(self.payload(), "running", "accepted")
            before = os.path.getsize(cli.OUTPUT_PATH)
            cli.record(
                {"hook_event_name": "UserPromptSubmit", "session_id": "x" * 500},
                "running",
                "accepted",
            )
        finally:
            cli.MAX_LOG_BYTES = original_limit
        self.assertEqual(os.path.getsize(cli.OUTPUT_PATH), before)
        lines = Path(cli.OUTPUT_PATH).read_text(encoding="utf-8").splitlines()
        self.assertEqual(len(lines), 1)

    def test_hook_log_concurrent_writers_stay_capped_and_valid(self):
        original_limit = cli.MAX_LOG_BYTES
        cli.MAX_LOG_BYTES = 4096
        errors = []
        try:
            def write_many(worker):
                try:
                    for index in range(50):
                        cli.record(
                            self.payload(turn=f"turn-{worker}-{index}"),
                            "running",
                            "accepted",
                        )
                except Exception as error:
                    errors.append(error)

            threads = [
                threading.Thread(target=write_many, args=(worker,)) for worker in range(8)
            ]
            for thread in threads:
                thread.start()
            for thread in threads:
                thread.join()
        finally:
            cli.MAX_LOG_BYTES = original_limit
        self.assertEqual(errors, [])
        self.assertLessEqual(os.path.getsize(cli.OUTPUT_PATH), 4096)
        lines = Path(cli.OUTPUT_PATH).read_text(encoding="utf-8").splitlines()
        self.assertGreater(len(lines), 0)
        for line in lines:
            json.loads(line)

    def test_hook_log_failures_stay_best_effort(self):
        original_lock = cli._lock_log_file
        cli._lock_log_file = lambda *_args: (_ for _ in ()).throw(
            OSError("lock unavailable")
        )
        try:
            self.assertFalse(cli.record_best_effort(self.payload(), "running", "accepted"))
            output = io.StringIO()
            with contextlib.redirect_stdout(output), contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(
                    cli.handle_payload(self.subagent_payload("SubagentStop")), 0
                )
            self.assertEqual(output.getvalue(), '{"continue":true}\n')
        finally:
            cli._lock_log_file = original_lock
        original_ftruncate = os.ftruncate
        original_limit = cli.MAX_LOG_BYTES
        cli.MAX_LOG_BYTES = 300
        os.ftruncate = lambda *_args, **_kwargs: (_ for _ in ()).throw(
            OSError("truncate failed")
        )
        try:
            cli.record(self.payload(), "running", "accepted")
            self.assertFalse(cli.record_best_effort(self.payload(turn="turn-b"), "running", "accepted"))
        finally:
            os.ftruncate = original_ftruncate
            cli.MAX_LOG_BYTES = original_limit
        lines = Path(cli.OUTPUT_PATH).read_text(encoding="utf-8").splitlines()
        self.assertEqual(len(lines), 1)
        self.assertEqual(json.loads(lines[0])["turn_id"], "turn-a")

    def test_hook_log_write_failure_stays_best_effort(self):
        original_write = os.write
        os.write = lambda *_args, **_kwargs: (_ for _ in ()).throw(
            OSError("write failed")
        )
        try:
            self.assertFalse(cli.record_best_effort(self.payload(), "running", "accepted"))
        finally:
            os.write = original_write
        self.assertTrue(Path(cli.OUTPUT_PATH).exists())
        self.assertEqual(os.path.getsize(cli.OUTPUT_PATH), 0)

    def test_session_end_clears_desired_state_after_unbind(self):
        bridge.send_subagent_start(self.subagent_payload())
        bridge.send_display_state(self.payload(), "running", None)
        cli.handle_payload(self.payload("SessionEnd"))
        state = storage.load_state("session-a")
        assert state is not None
        self.assertIsNone(state["main"])
        self.assertEqual(state["subagents"], {})
        self.assertIsNone(storage.load_active_session())

    def test_invalid_state_is_replaced_and_logged_as_degraded(self):
        storage.ensure_state_dir()
        Path(storage.state_path("session-a")).write_text(
            '{"session_id":"session-a","stale":true}\n', encoding="utf-8"
        )
        bridge.send_display_state(
            self.payload(), "running", None, diagnostic=cli.record_best_effort
        )
        log_lines = Path(cli.OUTPUT_PATH).read_text(encoding="utf-8").splitlines()
        events = [json.loads(line) for line in log_lines]
        self.assertTrue(
            any(
                event.get("bridge_event") == "state_reset"
                and event.get("display_delivery") == "degraded"
                for event in events
            )
        )

    def test_invalid_state_recovers_when_logging_fails(self):
        storage.ensure_state_dir()
        Path(storage.state_path("session-a")).write_text(
            '{"session_id":"session-a","stale":true}\n', encoding="utf-8"
        )
        original_record = cli.record
        cli.record = lambda *_args, **_kwargs: (_ for _ in ()).throw(
            OSError("disk full")
        )
        try:
            with contextlib.redirect_stderr(io.StringIO()):
                bridge.send_display_state(self.payload(), "running", None)
        finally:
            cli.record = original_record

        state = storage.load_state("session-a")
        assert state is not None
        self.assertEqual(state["main"]["state"], "running")


class HookConfigTest(unittest.TestCase):
    def commands(self):
        config = json.loads(HOOKS_PATH.read_text(encoding="utf-8"))
        return config, [
            hook["command"]
            for groups in config["hooks"].values()
            for group in groups
            for hook in group["hooks"]
        ]

    def test_all_hooks_use_the_same_bridge_command(self):
        config, commands = self.commands()
        self.assertEqual(len(commands), 10)
        self.assertEqual(len(set(commands)), 1)
        self.assertEqual(
            set(config["hooks"]),
            {
                "SessionStart",
                "UserPromptSubmit",
                "PermissionRequest",
                "PreToolUse",
                "PostToolUse",
                "SubagentStart",
                "SubagentStop",
                "Stop",
                "Interrupt",
                "SessionEnd",
            },
        )
        for groups in config["hooks"].values():
            self.assertEqual(len(groups), 1)
            self.assertNotIn("matcher", groups[0])
            self.assertEqual(len(groups[0]["hooks"]), 1)
        timeouts = {
            event_name: groups[0]["hooks"][0]["timeout"]
            for event_name, groups in config["hooks"].items()
        }
        self.assertEqual(timeouts["SessionStart"], 3)
        self.assertEqual(timeouts["Interrupt"], 3)
        self.assertEqual(timeouts["SessionEnd"], 3)
        self.assertTrue(
            all(
                timeout == 2
                for event_name, timeout in timeouts.items()
                if event_name not in {"SessionStart", "Interrupt", "SessionEnd"}
            )
        )

    def test_canonical_config_requires_installation_materialization(self):
        _, commands = self.commands()
        self.assertEqual(commands[0], "/usr/bin/python3 __AGENTFLARE_BRIDGE__")

    def test_subagent_stop_subprocess_returns_only_continue_json(self):
        with tempfile.TemporaryDirectory() as directory:
            environment = os.environ.copy()
            environment["AGENTFLARE_CODEX_HOOK_STATE_DIR"] = directory
            environment["AGENTFLARE_CODEX_HOOK_LOG"] = str(Path(directory) / "events.jsonl")
            completed = subprocess.run(
                [os.environ.get("PYTHON", "python3"), str(BRIDGE_PATH)],
                env=environment,
                input=json.dumps(
                    {
                        "hook_event_name": "SubagentStop",
                        "session_id": "session-a",
                        "turn_id": "turn-a",
                        "agent_id": "worker-a",
                        "agent_type": "worker",
                    }
                ),
                text=True,
                capture_output=True,
                check=False,
            )
            self.assertEqual(completed.returncode, 0, completed.stderr)
            self.assertEqual(completed.stdout, '{"continue":true}\n')


if __name__ == "__main__":
    unittest.main()
