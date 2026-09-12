# Firmware tools

Firmware tooling plus software-based tests. CI runs only
`test_build_direct_led.py` and `test_control_direct_led.py`; setup and CI
perform no USB, HID, DFU, flash, or other hardware operations. Hardware steps
stay explicit, owner-initiated actions from [../README.md](../README.md):
backup → build → check → flash → readback → LED check. Tools never reuse
stale device values or flash by themselves.
