#!/usr/bin/env python3
"""Tests for km16pro_control_direct_led.py using a fake HID transport.

No hardware access: a stub `hid` module answers VIA queries the way stock
firmware with the direct-LED patch does (protocol query, effect get/set,
direct-LED echo acknowledgement).
"""

import contextlib
import importlib.util
import io
import sys
import unittest
from pathlib import Path
from unittest import mock


PATH = Path(__file__).with_name("km16pro_control_direct_led.py")
SPEC = importlib.util.spec_from_file_location("km16pro_control_direct_led", PATH)
assert SPEC is not None and SPEC.loader is not None
control = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = control
SPEC.loader.exec_module(control)


def padded(payload: bytes) -> bytes:
    return payload.ljust(32, b"\0")


class FakeHidDevice:
    """Minimal hid.device stub answering VIA payloads."""

    def __init__(self, protocol: int = 12, effect: int = 0) -> None:
        self.protocol_version = protocol
        self.effect = effect
        self.sent_payloads: list[bytes] = []
        self.closed = False

    def open_path(self, path: object) -> None:
        _ = path

    def close(self) -> None:
        self.closed = True

    def write(self, report: bytes) -> int:
        self.sent_payloads.append(bytes(report[1:33]))
        return len(report)

    def read(self, size: int, timeout_ms: int = 0) -> bytes:
        _ = (size, timeout_ms)
        payload = self.sent_payloads[-1]
        if payload[0] == 0x01:
            response = bytes((0x01,)) + self.protocol_version.to_bytes(2, "big")
        elif payload[0] == 0x08:
            response = bytes(payload[:3]) + bytes((self.effect,))
        elif payload[0] == 0x09:
            self.effect = payload[3]
            response = bytes(payload[:4])
        else:
            response = bytes(payload[:7])
        return response.ljust(32, b"\0")


class FakeHidModule:
    def __init__(self, protocol: int = 12, effect: int = 0) -> None:
        self.protocol = protocol
        self.effect = effect
        self.last_device: FakeHidDevice | None = None

    def enumerate(self, vendor_id: int, product_id: int) -> list[dict]:
        _ = (vendor_id, product_id)
        return [{"usage_page": 0xFF60, "usage": 0x61, "path": b"fake"}]

    def device(self) -> FakeHidDevice:
        self.last_device = FakeHidDevice(self.protocol, self.effect)
        return self.last_device


class ControlDirectLedTest(unittest.TestCase):
    def open_fake(self, effect: int = 0) -> tuple[control.ViaDevice, FakeHidDevice]:
        fake_module = FakeHidModule(effect=effect)
        sys.modules["hid"] = fake_module
        self.addCleanup(sys.modules.pop, "hid", None)
        device = control.ViaDevice.open_one()
        self.addCleanup(device.close)
        assert fake_module.last_device is not None
        return device, fake_module.last_device

    def run_main(self, argv: list[str], effect: int = 0) -> tuple[int, str, FakeHidModule]:
        fake_module = FakeHidModule(effect=effect)
        sys.modules["hid"] = fake_module
        self.addCleanup(sys.modules.pop, "hid", None)
        output = io.StringIO()
        with mock.patch.object(sys, "argv", argv):
            with contextlib.redirect_stdout(output):
                status = control.main()
        return status, output.getvalue(), fake_module

    def test_temporary_clears_led_when_matrix_was_off(self) -> None:
        """Regression test: with effect 0 nothing repaints, so the LED
        must be reset to black explicitly or the color stays latched."""
        device, handle = self.open_fake(effect=0)
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            control.run_temporary(device, (control.direct_led_packet(7, 255, 0, 0),), 0)
        self.assertIn("direct LED ACK: 07 00 01 07 ff 00 00", output.getvalue())
        self.assertEqual(
            handle.sent_payloads,
            [
                padded(bytes((0x08, 0x03, 0x02))),
                padded(control.direct_led_packet(7, 255, 0, 0)),
                padded(control.direct_led_packet(7, 0, 0, 0)),
            ],
        )
        self.assertEqual(handle.effect, 0)

    def test_temporary_clears_led_and_restores_previous_effect(self) -> None:
        device, handle = self.open_fake(effect=5)
        with contextlib.redirect_stdout(io.StringIO()):
            control.run_temporary(device, (control.direct_led_packet(7, 255, 0, 0),), 0)
        self.assertEqual(
            handle.sent_payloads,
            [
                padded(bytes((0x08, 0x03, 0x02))),
                padded(bytes((0x09, 0x03, 0x02, 0x00))),
                padded(control.direct_led_packet(7, 255, 0, 0)),
                padded(control.direct_led_packet(7, 0, 0, 0)),
                padded(bytes((0x09, 0x03, 0x02, 0x05))),
            ],
        )
        self.assertEqual(handle.effect, 5)

    def test_hold_clears_led_and_restores_effect(self) -> None:
        device, handle = self.open_fake(effect=5)
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            control.hold_led(device, 7, 255, 0, 0, duration=0, interval=0.001)
        self.assertIn("direct LED writes: 0", output.getvalue())
        self.assertEqual(
            handle.sent_payloads,
            [
                padded(bytes((0x08, 0x03, 0x02))),
                padded(bytes((0x09, 0x03, 0x02, 0x00))),
                padded(control.direct_led_packet(7, 0, 0, 0)),
                padded(bytes((0x09, 0x03, 0x02, 0x05))),
            ],
        )
        self.assertEqual(handle.effect, 5)

    def test_off_turns_off_all_leds_without_index(self) -> None:
        status, output, fake_module = self.run_main(["prog", "--execute", "off"], effect=5)
        self.assertEqual(status, 0)
        self.assertIn("Turned 27 LED(s) black.", output)
        assert fake_module.last_device is not None
        handle = fake_module.last_device
        blacks = [padded(control.direct_led_packet(index, 0, 0, 0)) for index in range(27)]
        # `off` leaves the matrix paused like `set`: no effect restore follows.
        self.assertEqual(handle.sent_payloads[2:], blacks)
        self.assertEqual(handle.effect, 0)
        self.assertIn("Previous effect was 5", output)

    def test_off_single_index_leaves_matrix_paused(self) -> None:
        status, output, fake_module = self.run_main(["prog", "--execute", "off", "7"], effect=0)
        self.assertEqual(status, 0)
        self.assertIn("Turned 1 LED(s) black.", output)
        self.assertIn("Previous effect was 0", output)
        assert fake_module.last_device is not None
        handle = fake_module.last_device
        self.assertIn(padded(control.direct_led_packet(7, 0, 0, 0)), handle.sent_payloads)
        self.assertEqual(handle.effect, 0)

    def test_off_rejects_out_of_range_index(self) -> None:
        with contextlib.redirect_stdout(io.StringIO()):
            with mock.patch.object(sys, "argv", ["prog", "off", "27"]):
                with self.assertRaises(ValueError):
                    control.main()

    def test_probe_reports_protocol_and_matrix_effect(self) -> None:
        status, output, _ = self.run_main(["prog", "probe"], effect=0)
        self.assertEqual(status, 0)
        self.assertIn("VIA protocol: 12 (0x000c)", output)
        self.assertIn("RGB Matrix effect: 0 (off)", output)

    def test_selftest_lights_each_led_then_clears_all(self) -> None:
        device, handle = self.open_fake(effect=0)
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            control.run_selftest(device, 255, 0, 0, 0)
        colors = [padded(control.direct_led_packet(index, 255, 0, 0)) for index in range(27)]
        blacks = [padded(control.direct_led_packet(index, 0, 0, 0)) for index in range(27)]
        self.assertEqual(
            handle.sent_payloads,
            [padded(bytes((0x08, 0x03, 0x02))), *colors, *blacks],
        )
        self.assertEqual(handle.effect, 0)
        self.assertIn("selftest LED  0: ACK 07 00 01 00 ff 00 00", output.getvalue())
        self.assertIn(
            "selftest complete: 27/27 LEDs acknowledged, all cleared.", output.getvalue()
        )

    def test_selftest_restores_previous_effect(self) -> None:
        device, handle = self.open_fake(effect=3)
        with contextlib.redirect_stdout(io.StringIO()):
            control.run_selftest(device, 255, 0, 0, 0)
        colors = [padded(control.direct_led_packet(index, 255, 0, 0)) for index in range(27)]
        blacks = [padded(control.direct_led_packet(index, 0, 0, 0)) for index in range(27)]
        self.assertEqual(
            handle.sent_payloads,
            [
                padded(bytes((0x08, 0x03, 0x02))),
                padded(bytes((0x09, 0x03, 0x02, 0x00))),
                *colors,
                *blacks,
                padded(bytes((0x09, 0x03, 0x02, 0x03))),
            ],
        )
        self.assertEqual(handle.effect, 3)

    def test_selftest_via_main_execute(self) -> None:
        status, output, fake_module = self.run_main(
            ["prog", "--execute", "selftest", "--seconds", "0.001"], effect=0
        )
        self.assertEqual(status, 0)
        self.assertIn(
            "selftest complete: 27/27 LEDs acknowledged, all cleared.", output
        )
        assert fake_module.last_device is not None
        self.assertEqual(fake_module.last_device.effect, 0)

    def test_selftest_rejects_nonpositive_seconds(self) -> None:
        with contextlib.redirect_stdout(io.StringIO()):
            with contextlib.redirect_stderr(io.StringIO()):
                with mock.patch.object(sys, "argv", ["prog", "selftest", "--seconds", "0"]):
                    with self.assertRaises(SystemExit):
                        control.main()


if __name__ == "__main__":
    unittest.main()
