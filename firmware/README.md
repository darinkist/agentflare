# Firmware workspace

One supported path: back up stock, build the Direct-LED patch, flash it,
verify it, then control LEDs. Anything else is unsupported.

1. [Install](#1-install-the-tools) the tools.
2. [Back up](#3-back-up-the-stock-application) stock twice and verify.
3. [Build](#4-build-the-patch) the patch, then run the
   [pre-flight check](#5-pre-flight-check).
4. [Flash](#6-deliberate-write) once, then
   [read back](#7-read-back-the-written-region) and verify.
5. [Run the automatic LED self-test](#8-run-the-automatic-led-self-test);
   [restore](#9-restore-the-original-firmware) only from a verified backup.

Public source (committed): the patch in `patch/direct-led/` and the scripts in
`tools/`. Local-only, never committed: `backups-local/`, `builds-local/`,
`evidence/`, `archive/`. Read the procedure below before any hardware work.
The daemon socket `V2` API is unrelated to firmware naming.

---

This is an owner-operated, recovery-sensitive procedure. It is not part of
normal AgentFlare setup. It supports only a KM16-Pro in USB mode and the exact
application backup accepted by `km16pro_build_direct_led.py`. It does not
establish support for other revisions, bootloaders, firmware generations, or
wireless operation. macOS-only; other systems are unverified.

Do not continue unless you own the keyboard, have a second verified backup, and
accept that a failed flash may require manufacturer support or hardware repair.
No command below is run by setup or CI.

## 0. Overview (~15 minutes)

1. Install the tools (~5 min).
2. Enter bootloader mode and identify the target (~2 min).
3. Back up the stock application twice and verify (~5 min).
4. Build the patch and verify (~2 min).
5. Run the pre-flight check (~1 min).
6. Flash once with a manual command.
7. Read back and verify the written region (~3 min).
8. Run the automatic LED self-test.
9. Know the restore path before you need it. If anything goes wrong, see
   Troubleshooting, then restore.

Stop on anything unexpected at any step. Never reuse values from an earlier
`dfu-util -l` output in a later step. Run all commands from the repository
root, so relative `firmware/…` paths resolve.

## DFU quirk and stop rules

`dfu-util` may end an otherwise complete 122880-byte upload with
`LIBUSB_ERROR_PIPE` or “Error during upload”. This is acceptable **only** when
the tool reports all 122880 bytes received, the saved file is exactly 122880
bytes, and every other check for that step passes. Anything else is a stop
condition:

| Symptom | Action |
|---|---|
| Short file, reads differ, build/check refusal | Stop, keep all files |
| More than one (or zero) DFU targets with a `0x08002000` area | Stop, contact seller/manufacturer |
| Any error besides the narrow quirk above | Stop |
| Uncertain write, readback, or normal operation | Stop, go to step 9 |

## 1. Install the tools

```bash
xcode-select --install
# After installing Homebrew from https://brew.sh:
brew install python dfu-util uv
python3 --version
dfu-util --version
xcrun --find clang
uv --version
```

Expected result: each of the last four commands prints a version or path. The
builder uses Apple Clang; no Arduino tooling is needed. `uv` is needed only for
the LED check in step 8 (it provides `hidapi`).

## 2. Enter bootloader mode and identify the target

Use the vendor's current instructions if they differ. The only publicly
documented procedure for the related MMD KM16 lineage is to hold key 1
(upper-left key) while connecting the USB cable, then list DFU devices:

```bash
dfu-util -l
```

That procedure comes from the [upstream RawMacroPad documentation](https://github.com/toptensoftware/rawMacroPad),
not verified manufacturer documentation for every KM16-Pro revision. It is a
bootloader-entry reference only; the full-replacement RawMacroPad experiment is
archived, unsupported, and not used here.

Anatomy of the output (structure is real, values redacted — your listing
looks like this, with three alts on one device):

```text
Found DFU: [<VID:PID>] ...
  alt=<ALT>, name="STM32duino bootloader v1.0  Upload to Flash 0x8002000", ...
  alt=..., name="STM32duino bootloader v1.0  Upload to Flash 0x8005000", ...
  alt=..., name="STM32duino bootloader v1.0  ERROR. Upload to RAM not supported.", ...
```

Decision rule — match the name, ignore everything else:

| Name contains | Meaning | Action |
|---|---|---|
| `0x8002000` (same address as `0x08002000`, fewer leading zeroes) | Application area | **Take this `alt`** |
| `0x8005000` | A different flash region | Never use it here |
| `ERROR` / `RAM` | Not a flash target | Never use it |

* `-d` takes the **VID:PID selector** (for example `1eaf:0003`), never `intf`,
  a path, or a serial number.
* `-a` takes the **`alt` value** from the application row, not `intf`.
* Expected result: exactly one device offers exactly one such `alt`. Otherwise
  stop. See the [dfu-util manual](https://dfu-util.sourceforge.net/dfu-util.1.html)
  for field meanings.

## 3. Back up the stock application

Preview first (no USB access), then run. Outputs default to
`firmware/backups-local/`, which is gitignored so blobs are never committed.
The keyboard must still be in bootloader mode: rerun `dfu-util -l` first and
stop if your target is gone (re-enter as in step 2). Type values with straight
quotes or none at all — macOS smart quotes (`‚2‘`) silently corrupt arguments;
bare `--selector 1eaf:0003 --alt 2` is safest:

```bash
python3 firmware/tools/km16pro_backup.py --selector '<VID:PID>' --alt '<ALT>' --name 'my-pad'
python3 firmware/tools/km16pro_backup.py --execute --selector '<VID:PID>' --alt '<ALT>' --name 'my-pad'
```

Every backup needs a name (`--name` writes `<name>-read-1.bin` and
`<name>-read-2.bin` into `firmware/backups-local/`); the only alternative is
explicit paths with `--first` and `--second` together. Re-running a name
refuses to overwrite — add `--overwrite` to replace deliberately.

Expected result: `two reads match`, plus size, SHA-256, and a verdict.
The quirk box above applies. Copy the backup to separate physical or
cloud-backed storage before continuing.

The backup always succeeds when the two reads match — the verdict tells you
what you got:

* `VERIFIED STOCK`: byte-identical to the tested reference image (122880
  bytes, hash
  `73b88b069a06c29b86d729fb7f1df1b36d3d36e4e72d4b0f8af2ba94e38540b8` from
  the author's revision). Proceed to step 4.
* `PATCH-COMPATIBLE LAYOUT`: the four instructions the patch replaces match
  the tested layout, while settings bytes differ (normal after boot, or
  another stock revision). Expected to build; proceed to step 4, the step 5
  check verifies fully before any flash.
* `DEVICE SNAPSHOT (unrecognized layout)`: the hook sites differ (custom
  firmware). The backup is kept as-is and restorable, but cannot be built
  from — build from a compatible backup instead.

No single hash covers every stock revision; that hash above is the tested
reference, not universal truth. Safety comes from verifying the exact bytes
being replaced, which the builder and the step 5 check both do.

A snapshot verdict right after you flashed stock is normal, not a failure:
the device rewrites a few settings bytes in flash on boot, so a readback is
expected to differ slightly from the file that was written. If you just
flashed a known file, prove it to yourself with `--reference`:

```bash
python3 firmware/tools/km16pro_backup.py --execute --selector '<VID:PID>' --alt '<ALT>' --name 'my-pad-check' --reference '<FILE-YOU-FLASHED>'
```

This adds a comparison line such as `31 of 122880 bytes differ, patch region
intact — stock code, device settings changed (normal after boot)`. `patch
region DIFFERS` instead means the code itself is not stock.

## 4. Build the patch

```bash
# BACKUP_ONE = input: the backup you created in step 3 (replace <NAME>).
# PATCH = output: the new modified firmware image (any name works; the
# following steps reuse $PATCH, so keep the terminal open).
export BACKUP_ONE='firmware/backups-local/<NAME>-read-1.bin'
export PATCH='firmware/builds-local/km16pro-direct-led.bin'
python3 firmware/tools/km16pro_build_direct_led.py --source "$BACKUP_ONE" --output "$PATCH"
stat -f%z "$PATCH"
shasum -a 256 "$PATCH"
```

Replace `<NAME>` with the `--name` you used in step 3. (`PATCH` is just the
output file for the following steps — any name works.)

Expected result: **57344 bytes**, SHA-256
`eda935bca0a1a743196011f8c814a15bf66c1cfc9bc3d0825bad9fe9606b559c`.
The output is shorter than the backup because only the first 57344-byte region
is rewritten. Anything else is a stop condition. The builder validates the
four hook-site instructions, the jump targets, and the erased handler area —
patch offsets are valid only where those match the tested layout. A whole-file
hash is intentionally not the gate: no single hash covers every stock revision.

## 5. Pre-flight check

```bash
python3 firmware/tools/km16pro_build_direct_led.py --check --backup "$BACKUP_ONE" --patch "$PATCH"
```

Expected result: `backup OK` and `patch OK` lines plus the exact manual flash
command. This check never touches USB. Fix anything it reports before
continuing.

## 6. Deliberate write

Re-enter bootloader mode as in step 2 (hold key 1 while connecting the USB
cable), rerun `dfu-util -l`, and set both variables from that
new output only:

```bash
export DFU_SELECTOR='<VID:PID>'
export DFU_APP_ALT='<ALT>'
dfu-util -d "$DFU_SELECTOR" -a "$DFU_APP_ALT" -D "$PATCH"
```

Wait for it to finish. The device restarts on success. Do not attempt a
readback on that old DFU connection; rediscover the device first (step 7).

## 7. Read back the written region

Enter bootloader mode again as in step 2 (hold key 1 while connecting the USB
cable), then:

```bash
dfu-util -l
export DFU_SELECTOR='<VID:PID>'
export DFU_APP_ALT='<ALT>'
export READBACK="firmware/builds-local/km16-readback-full.bin"
export READBACK_PATCH="firmware/builds-local/km16-readback-patch-region.bin"
rm -f "$READBACK" "$READBACK_PATCH"
dfu-util -d "$DFU_SELECTOR" -a "$DFU_APP_ALT" -U "$READBACK"
stat -f%z "$READBACK"
dd if="$READBACK" of="$READBACK_PATCH" bs=1 count=57344
cmp -s "$PATCH" "$READBACK_PATCH" && echo "written region matches"
```

Expected result: `READBACK` is **122880 bytes**; `READBACK_PATCH` and `PATCH`
are **57344 bytes**; final line prints `written region matches`. The quirk box
applies to the full read. The original-firmware hash does not apply after
flashing. Do not compare the patch directly against the full readback.
`dfu-util` refuses to overwrite an existing file (`Cannot open file ...
File exists`) and reports `No DFU capable USB device available` outside
bootloader mode — in both cases the `stat`/`dd`/`cmp` lines below re-check
stale files, so do not trust `written region matches`. Delete the stale
outputs, re-enter bootloader mode, take fresh values, then read again.

## 8. Run the automatic LED self-test

The keyboard is back in normal mode (USB `28e9:3145`) here, not DFU mode.

```bash
uv run --with hidapi python firmware/tools/km16pro_control_direct_led.py probe
uv run --with hidapi python firmware/tools/km16pro_control_direct_led.py --execute selftest
```

Expected result: `VIA protocol: …` and `RGB Matrix effect: …` lines (stock
backup reported protocol 12), then one `selftest LED …: ACK 07 00 01 …` line
per LED while each lights up in turn, and finally
`selftest complete: 27/27 LEDs acknowledged, all cleared.` — afterwards
everything is off again by itself, no manual cleanup needed. Setting commands
are dry-runs without `--execute`. A missing device here is a stop condition,
not a reason to reflash blindly.

For a single LED instead of the full run:

```bash
uv run --with hidapi python firmware/tools/km16pro_control_direct_led.py --execute demo 7 255 0 0
```

Temporary commands reset their LEDs to black afterwards, so nothing stays
lit. To turn LEDs off manually (one, several, or all 27 without an index):

```bash
uv run --with hidapi python firmware/tools/km16pro_control_direct_led.py --execute off 7
```

## 9. Restore the original firmware

Only from the verified backup in step 3. Enter bootloader mode as in step 2
(hold key 1 while connecting the USB cable), then after fresh discovery:

```bash
dfu-util -l
export DFU_SELECTOR='<VID:PID>'
export DFU_APP_ALT='<ALT>'
export RESTORE='firmware/backups-local/<NAME>-read-1.bin'
stat -f%z "$RESTORE"
shasum -a 256 "$RESTORE"
dfu-util -d "$DFU_SELECTOR" -a "$DFU_APP_ALT" -D "$RESTORE"
```

Replace `<NAME>` with the backup you want back on the device (either read
works; they are byte-identical).

Expected result: `RESTORE` is **122880 bytes** with hash
`73b88b069a06c29b86d729fb7f1df1b36d3d36e4e72d4b0f8af2ba94e38540b8` before the
write; the device restarts afterwards. If anything is uncertain, stop. Do not
send LED cleanup commands, repeat writes, or reuse stale paths.

Verify the restore: enter bootloader mode again as in step 2, read the full
application area once more, and compare the code region against the backup.
A full-image compare is not used here on purpose: the device rewrites a few
settings bytes near the end of flash on boot (see step 3), so a correct
restore still differs there after the first restart:

```bash
dfu-util -l
export DFU_SELECTOR='<VID:PID>'
export DFU_APP_ALT='<ALT>'
export VERIFY="firmware/builds-local/km16-restore-verify.bin"
export VERIFY_CODE="firmware/builds-local/km16-restore-verify-code.bin"
export RESTORE_CODE="firmware/builds-local/km16-restore-code.bin"
rm -f "$VERIFY" "$VERIFY_CODE" "$RESTORE_CODE"
dfu-util -d "$DFU_SELECTOR" -a "$DFU_APP_ALT" -U "$VERIFY"
stat -f%z "$VERIFY"
dd if="$VERIFY" of="$VERIFY_CODE" bs=1 count=57344
dd if="$RESTORE" of="$RESTORE_CODE" bs=1 count=57344
cmp -s "$RESTORE_CODE" "$VERIFY_CODE" && echo "restore verified"
```

Expected result: `VERIFY` is **122880 bytes**, both extracted regions are
**57344 bytes**, and the final line prints `restore verified`. The quirk box
applies to the read. Then unplug, reconnect normally (no key held), and
confirm the keyboard returns as USB `28e9:3145`.

## Troubleshooting

Reads are always safe to retry (with fresh discovery). Writes are not: never
repeat a write to "fix" an uncertain one — restore once per step 9, then verify.

| Symptom | Likely cause | What to do |
|---|---|---|
| `dfu-util -l` shows no DFU device | Key-1 timing, cable, or port | Retry entry once (hold key 1 before and during plug-in); try another cable/port; stop if still absent |
| Zero or multiple `0x08002000` targets | Wrong device or hub interference | Disconnect other DFU/USB devices, rediscover; stop if still ambiguous |
| Two backup reads differ or are short | Unstable transfer | Fresh discovery, read again; stop if it repeats, keep both files |
| Reads match but verdict is a device snapshot | Custom firmware (hook sites differ) | Backup is fine and restorable; build only from a compatible backup |
| Patch size/hash is not 57344 B / `eda935…` | Wrong source or disturbed build | Stop; do not flash |
| Pre-flight check fails | Any of the above | Stop; fix what it reports |
| Write interrupted (cable, error, doubt) | Incomplete flash | Re-enter bootloader, fresh discovery, restore once per step 9, then verify |
| No USB device at all after flashing | Uncertain flash result | Try bootloader entry on another port/cable; if the bootloader appears, restore per step 9; if nothing appears anywhere, stop and contact the seller/manufacturer |
| Normal mode (`28e9:3145`) back but LEDs ignore commands | Effect repaint or connection issue | Re-run `probe`; do not reflash blindly; restore per step 9 if uncertain |
| LED stays on after `demo`/`hold`/`walk` | Should no longer happen (LEDs reset to black first); older script version | Update the script; switch off manually with `--execute off <index>` |
| Readback region mismatch | Write did not stick | Stop; restore per step 9, then verify |
| Snapshot verdict right after flashing stock | Boot-rewritten settings bytes, not a bad flash | Re-run with `--reference` pointing at the flashed file; `patch region intact` means stock code |
| Keys dead on patched firmware but LED self-test ACKs | HID interaction or settings/keymap storage, not the LED hooks | Stop the daemon, test typing with the daemon stopped. Keys back means daemon interaction; still dead means restore per step 9 with no extra writes, and keep the readback as evidence |
| No physical colour on stock with daemon running | Expected: individual LED control exists only on the Direct-LED patch | Not a failure. `status --v2 --json` confirms the accepted socket state only, never physical LEDs |

## Licence

The [MIT license](../LICENSE) covers original AgentFlare code. Vendor firmware,
derived images, third-party tools, and device warranties may have separate
terms. This repository grants no right to redistribute them.
