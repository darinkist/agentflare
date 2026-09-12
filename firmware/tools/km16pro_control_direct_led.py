#!/usr/bin/env python3
"""Direct control for the KM16 Pro direct-LED firmware patch.

The script speaks the existing VIA HID interface (USB 28e9:3145).  It does
not use VIA's persistent save command.  Commands which set an LED first pause
the RGB Matrix effect because that effect would otherwise repaint the LED.

Examples:
  uv run --with hidapi python km16pro_control_direct_led.py probe
  uv run --with hidapi python km16pro_control_direct_led.py set 7 255 0 0
  uv run --with hidapi python km16pro_control_direct_led.py --execute demo 7 255 0 0
  uv run --with hidapi python km16pro_control_direct_led.py --execute hold 7 255 0 0
  uv run --with hidapi python km16pro_control_direct_led.py --execute walk
  uv run --with hidapi python km16pro_control_direct_led.py --execute off 7
  uv run --with hidapi python km16pro_control_direct_led.py --execute selftest

Temporary commands (demo/hold/walk) reset their LEDs to black before
restoring the previous effect, so no color stays latched when the matrix
effect was already off. `off` turns LEDs black immediately. `selftest`
lights each of the 27 LEDs in turn and switches everything off again, so a
single call checks the whole pad.

Setting commands are dry-runs until --execute is supplied.
"""

from __future__ import annotations

import argparse
import sys
import time
from dataclasses import dataclass
from typing import Iterable


VENDOR_ID = 0x28E9
PRODUCT_ID = 0x3145
USAGE_PAGE = 0xFF60
USAGE = 0x61
REPORT_BYTES = 32
RGB_MATRIX_CHANNEL = 3
RGB_MATRIX_EFFECT_VALUE = 2
RGB_MATRIX_NONE = 0
DIRECT_LED_COMMAND = (0x07, 0x00, 0x01)


def ensure_byte(value: int, label: str) -> int:
    if not 0 <= value <= 0xFF:
        raise ValueError(f"{label} must be in 0..255, got {value}")
    return value


def direct_led_packet(index: int, red: int, green: int, blue: int) -> bytes:
    if not 0 <= index <= 26:
        raise ValueError(f"LED index must be in 0..26, got {index}")
    return bytes((*DIRECT_LED_COMMAND, index, ensure_byte(red, "red"),
                  ensure_byte(green, "green"), ensure_byte(blue, "blue")))


def via_get(channel: int, value_id: int) -> bytes:
    return bytes((0x08, channel, value_id))


def via_set(channel: int, value_id: int, value: int) -> bytes:
    return bytes((0x09, channel, value_id, ensure_byte(value, "value")))


@dataclass
class ViaDevice:
    device: object

    @classmethod
    def open_one(cls) -> "ViaDevice":
        try:
            import hid
        except ModuleNotFoundError as exc:
            raise RuntimeError("hidapi is required. Use: uv run --with hidapi python …") from exc

        matches = [
            entry for entry in hid.enumerate(VENDOR_ID, PRODUCT_ID)
            if entry.get("usage_page") == USAGE_PAGE and entry.get("usage") == USAGE
        ]
        if len(matches) != 1:
            raise RuntimeError(
                f"Expected exactly one KM16 Pro VIA interface ({VENDOR_ID:04x}:{PRODUCT_ID:04x}), "
                f"found {len(matches)}. Disconnect other matching devices."
            )
        handle = hid.device()
        handle.open_path(matches[0]["path"])
        return cls(handle)

    def close(self) -> None:
        self.device.close()

    def exchange(self, payload: bytes, *, timeout_ms: int = 1500) -> bytes:
        if len(payload) > REPORT_BYTES:
            raise ValueError("VIA payload exceeds 32 bytes")
        report = bytes((0,)) + payload.ljust(REPORT_BYTES, b"\0")
        written = self.device.write(report)
        if written != len(report):
            raise OSError(f"Incomplete HID write: expected {len(report)}, wrote {written}")
        response = bytes(self.device.read(REPORT_BYTES, timeout_ms))
        if len(response) != REPORT_BYTES:
            raise TimeoutError(f"Expected a 32-byte VIA response, received {len(response)} bytes")
        return response

    def query(self, payload: bytes) -> bytes:
        response = self.exchange(payload)
        if response[:len(payload)] != payload:
            raise RuntimeError(
                f"Unexpected VIA response: sent {payload.hex(' ')}, got {response.hex(' ')}"
            )
        return response

    def protocol(self) -> int:
        response = self.query(bytes((0x01,)))
        return int.from_bytes(response[1:3], "big")

    def current_rgb_matrix_effect(self) -> int:
        return self.query(via_get(RGB_MATRIX_CHANNEL, RGB_MATRIX_EFFECT_VALUE))[3]

    def set_rgb_matrix_effect(self, effect: int) -> None:
        # Consume the setter's reply so it cannot be mistaken for the following
        # direct-LED acknowledgement. We deliberately do not require its shape.
        self.exchange(via_set(RGB_MATRIX_CHANNEL, RGB_MATRIX_EFFECT_VALUE, effect))

    def set_led(self, index: int, red: int, green: int, blue: int) -> bytes:
        packet = direct_led_packet(index, red, green, blue)
        response = self.exchange(packet)
        # The patch intentionally leaves a valid request unchanged. The VIA
        # transport returns that packet, which is our acknowledgement.
        if response[:len(packet)] != packet:
            raise RuntimeError(
                f"Direct LED command was not acknowledged: sent {packet.hex(' ')}, "
                f"got {response.hex(' ')}"
            )
        return response


def pause_matrix(device: ViaDevice) -> int:
    previous_effect = device.current_rgb_matrix_effect()
    if previous_effect != RGB_MATRIX_NONE:
        device.set_rgb_matrix_effect(RGB_MATRIX_NONE)
        time.sleep(0.1)
    return previous_effect


def restore_matrix(device: ViaDevice, effect: int) -> None:
    if effect != RGB_MATRIX_NONE:
        device.set_rgb_matrix_effect(effect)


def print_dry_run(packets: Iterable[bytes]) -> None:
    for packet in packets:
        print(f"DRY-RUN  {packet.hex(' ')}")


def clear_leds(device: ViaDevice, indices: Iterable[int]) -> None:
    """Reset LEDs to black so no color stays latched.

    Required when the RGB Matrix effect was already off: nothing would
    repaint the LEDs after a temporary command, leaving the last color on.
    """
    for index in dict.fromkeys(indices):
        device.set_led(index, 0, 0, 0)


def run_temporary(device: ViaDevice, packets: Iterable[bytes], pause_seconds: float) -> None:
    packets = tuple(packets)
    previous_effect = pause_matrix(device)
    try:
        for packet in packets:
            index, red, green, blue = packet[3:7]
            response = device.set_led(index, red, green, blue)
            print(f"direct LED ACK: {response[:7].hex(' ')}", flush=True)
            time.sleep(pause_seconds)
    finally:
        clear_leds(device, (packet[3] for packet in packets))
        restore_matrix(device, previous_effect)


def hold_led(device: ViaDevice, index: int, red: int, green: int, blue: int,
             duration: float, interval: float) -> None:
    """Continuously reapply a pixel while the matrix task is paused.

    This is primarily a diagnostic for firmware builds where the RGB task may
    repaint the buffer between individual host commands.
    """
    previous_effect = pause_matrix(device)
    try:
        deadline = time.monotonic() + duration
        writes = 0
        while time.monotonic() < deadline:
            device.set_led(index, red, green, blue)
            writes += 1
            time.sleep(interval)
        print(f"direct LED writes: {writes}")
    finally:
        clear_leds(device, (index,))
        restore_matrix(device, previous_effect)


def run_selftest(device: ViaDevice, red: int, green: int, blue: int, seconds: float) -> None:
    """Light each LED in turn, then switch everything off and restore the effect.

    A single call exercises the whole pad: every LED is set once (a missing
    acknowledgement raises), then all LEDs are cleared, so nothing stays lit
    and no manual `off` is needed afterwards.
    """
    previous_effect = pause_matrix(device)
    try:
        for index in range(27):
            response = device.set_led(index, red, green, blue)
            print(f"selftest LED {index:2d}: ACK {response[:7].hex(' ')}", flush=True)
            time.sleep(seconds)
    finally:
        clear_leds(device, range(27))
        restore_matrix(device, previous_effect)
    print("selftest complete: 27/27 LEDs acknowledged, all cleared.")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--execute", action="store_true",
        help="Actually send a setting command. Without this, set/demo/hold/walk/effect/off/selftest are dry-runs.",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)
    subparsers.add_parser(
        "probe",
        help="Read the VIA protocol version and current RGB Matrix effect. No settings are changed.",
    )

    set_parser = subparsers.add_parser("set", help="Set one LED and leave RGB Matrix paused.")
    set_parser.add_argument("index", type=int)
    set_parser.add_argument("red", type=int)
    set_parser.add_argument("green", type=int)
    set_parser.add_argument("blue", type=int)

    demo_parser = subparsers.add_parser("demo", help="Temporarily set one LED, then restore its effect.")
    demo_parser.add_argument("index", type=int)
    demo_parser.add_argument("red", type=int)
    demo_parser.add_argument("green", type=int)
    demo_parser.add_argument("blue", type=int)
    demo_parser.add_argument("--seconds", type=float, default=2.0)

    hold_parser = subparsers.add_parser("hold", help="Continuously apply one LED, then restore its effect.")
    hold_parser.add_argument("index", type=int)
    hold_parser.add_argument("red", type=int)
    hold_parser.add_argument("green", type=int)
    hold_parser.add_argument("blue", type=int)
    hold_parser.add_argument("--seconds", type=float, default=5.0)
    hold_parser.add_argument("--interval", type=float, default=0.02)

    walk_parser = subparsers.add_parser("walk", help="Walk all 27 LEDs in red, then restore the effect.")
    walk_parser.add_argument("--seconds", type=float, default=1.0)
    walk_parser.add_argument("--red", type=int, default=48)
    walk_parser.add_argument("--green", type=int, default=0)
    walk_parser.add_argument("--blue", type=int, default=0)

    effect_parser = subparsers.add_parser("effect", help="Set the normal RGB Matrix effect explicitly.")
    effect_parser.add_argument("value", type=int, choices=range(256), metavar="0..255")

    off_parser = subparsers.add_parser(
        "off", help="Turn LEDs black and leave RGB Matrix paused. No index turns off all 27 LEDs."
    )
    off_parser.add_argument("indices", type=int, nargs="*", metavar="index")

    selftest_parser = subparsers.add_parser(
        "selftest",
        help="Light each of the 27 LEDs in turn, then switch all off and restore the effect.",
    )
    selftest_parser.add_argument("--seconds", type=float, default=0.2)
    selftest_parser.add_argument("--red", type=int, default=255)
    selftest_parser.add_argument("--green", type=int, default=0)
    selftest_parser.add_argument("--blue", type=int, default=0)
    args = parser.parse_args()

    if args.command == "probe":
        device = ViaDevice.open_one()
        try:
            protocol = device.protocol()
            effect = device.current_rgb_matrix_effect()
            print(f"VIA protocol: {protocol} (0x{protocol:04x})")
            print(f"RGB Matrix effect: {effect}" + (" (off)" if effect == RGB_MATRIX_NONE else ""))
        finally:
            device.close()
        return 0

    if args.command == "set":
        packet = direct_led_packet(args.index, args.red, args.green, args.blue)
        if not args.execute:
            print_dry_run((via_get(RGB_MATRIX_CHANNEL, RGB_MATRIX_EFFECT_VALUE),
                           via_set(RGB_MATRIX_CHANNEL, RGB_MATRIX_EFFECT_VALUE, RGB_MATRIX_NONE), packet))
            print("Would leave RGB Matrix paused after the direct LED command. Add --execute to send.")
            return 0
        device = ViaDevice.open_one()
        try:
            previous_effect = pause_matrix(device)
            response = device.set_led(args.index, args.red, args.green, args.blue)
            print(f"direct LED ACK: {response[:7].hex(' ')}")
            print(f"RGB Matrix remains paused. Previous effect was {previous_effect}; restore with: "
                  f"{PathHint.effect_command(previous_effect)}")
        finally:
            device.close()
        return 0

    if args.command == "demo":
        if args.seconds <= 0:
            parser.error("--seconds must be positive")
        packet = direct_led_packet(args.index, args.red, args.green, args.blue)
        if not args.execute:
            print_dry_run((via_get(RGB_MATRIX_CHANNEL, RGB_MATRIX_EFFECT_VALUE),
                           via_set(RGB_MATRIX_CHANNEL, RGB_MATRIX_EFFECT_VALUE, RGB_MATRIX_NONE), packet))
            print("Would restore the previously read RGB Matrix effect after the delay. Add --execute to send.")
            return 0
        device = ViaDevice.open_one()
        try:
            run_temporary(device, (packet,), args.seconds)
        finally:
            device.close()
        return 0

    if args.command == "hold":
        if args.seconds <= 0 or args.interval <= 0:
            parser.error("--seconds and --interval must be positive")
        packet = direct_led_packet(args.index, args.red, args.green, args.blue)
        if not args.execute:
            print_dry_run((via_get(RGB_MATRIX_CHANNEL, RGB_MATRIX_EFFECT_VALUE),
                           via_set(RGB_MATRIX_CHANNEL, RGB_MATRIX_EFFECT_VALUE, RGB_MATRIX_NONE), packet))
            print("Would repeatedly send this direct LED command before restoring the effect. Add --execute to send.")
            return 0
        device = ViaDevice.open_one()
        try:
            hold_led(device, args.index, args.red, args.green, args.blue, args.seconds, args.interval)
        finally:
            device.close()
        return 0

    if args.command == "walk":
        if args.seconds <= 0:
            parser.error("--seconds must be positive")
        packets = [direct_led_packet(index, args.red, args.green, args.blue) for index in range(27)]
        if not args.execute:
            print_dry_run((via_get(RGB_MATRIX_CHANNEL, RGB_MATRIX_EFFECT_VALUE),
                           via_set(RGB_MATRIX_CHANNEL, RGB_MATRIX_EFFECT_VALUE, RGB_MATRIX_NONE), *packets))
            print("Would restore the previously read RGB Matrix effect after the walk. Add --execute to send.")
            return 0
        device = ViaDevice.open_one()
        try:
            run_temporary(device, packets, args.seconds)
        finally:
            device.close()
        return 0

    if args.command == "effect":
        if not args.execute:
            print_dry_run((via_set(RGB_MATRIX_CHANNEL, RGB_MATRIX_EFFECT_VALUE, args.value),))
            print("Add --execute to send.")
            return 0
        device = ViaDevice.open_one()
        try:
            device.set_rgb_matrix_effect(args.value)
            print(f"RGB Matrix effect set to {args.value}.")
        finally:
            device.close()
        return 0

    if args.command == "off":
        indices = args.indices if args.indices else list(range(27))
        packets = [direct_led_packet(index, 0, 0, 0) for index in indices]
        if not args.execute:
            print_dry_run((via_get(RGB_MATRIX_CHANNEL, RGB_MATRIX_EFFECT_VALUE),
                           via_set(RGB_MATRIX_CHANNEL, RGB_MATRIX_EFFECT_VALUE, RGB_MATRIX_NONE),
                           *packets))
            print("Would leave RGB Matrix paused after turning the LEDs black. Add --execute to send.")
            return 0
        device = ViaDevice.open_one()
        try:
            previous_effect = pause_matrix(device)
            clear_leds(device, indices)
            print(f"Turned {len(packets)} LED(s) black.")
            print(f"RGB Matrix remains paused. Previous effect was {previous_effect}; restore with: "
                  f"{PathHint.effect_command(previous_effect)}")
        finally:
            device.close()
        return 0

    if args.command == "selftest":
        if args.seconds <= 0:
            parser.error("--seconds must be positive")
        packets = [direct_led_packet(index, args.red, args.green, args.blue) for index in range(27)]
        if not args.execute:
            print_dry_run((via_get(RGB_MATRIX_CHANNEL, RGB_MATRIX_EFFECT_VALUE),
                           via_set(RGB_MATRIX_CHANNEL, RGB_MATRIX_EFFECT_VALUE, RGB_MATRIX_NONE),
                           *packets))
            print("Would light each LED in turn, then switch all off and restore the effect. "
                  "Add --execute to send.")
            return 0
        device = ViaDevice.open_one()
        try:
            run_selftest(device, args.red, args.green, args.blue, args.seconds)
        finally:
            device.close()
        return 0

    raise AssertionError("unreachable")


class PathHint:
    @staticmethod
    def effect_command(effect: int) -> str:
        return ("uv run --with hidapi python firmware/tools/km16pro_control_direct_led.py "
                f"--execute effect {effect}")


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, RuntimeError, TimeoutError, ValueError) as exc:
        print(f"Error: {exc}", file=sys.stderr)
        raise SystemExit(2)
