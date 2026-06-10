# oasis-astro

Pure-Go drivers for **Astroasis Oasis** USB accessories — talking each device's HID
protocol directly, with no vendor SDK in the process:

- **`oasisfw/`** — Oasis filter wheel
- **`oasisfoc/`** — Oasis focuser (absolute, with temperature)

Both devices are driven over HID **interrupt** endpoints (`hid_write` / `hid_read_timeout`),
not feature reports, and share the same wire conventions: 65-byte reports, big-endian
numerics, and a set of per-OS transport backends. The cgo backend's C symbols are
prefixed per package, so both drivers can link into one binary.

```
command : [0]=0x00 reportID  [1]=opcode  [2]=payloadLen  [3..]=payload (padded to 65)
reply   : [0]=opcode echo    [1]=len     [2..]=data
```

Astroasis vendor ID is `0x338F`; the filter wheel is PID `0x0FE0`, the focuser PID
`0xA0F0`.

## Status

Both drivers have been exercised against real hardware. The pure-Go protocol logic is
also covered by `-race` tests over a fake transport, so it builds and tests with no
device and no cgo.

| | Filter wheel (`oasisfw`) | Focuser (`oasisfoc`) |
|---|---|---|
| USB ID | VID `0x338F` / PID `0x0FE0` | VID `0x338F` / PID `0xA0F0` |
| Identity | model, serial, hardware/firmware/protocol versions, firmware build date | model, serial, hardware/firmware/protocol versions, firmware build date |
| Read surface | status (position, state, temperature), slot count, per-slot names / focus offsets / colors, config | status (position, moving, internal + external temperature), config + extended config |
| Motion | go-to slot, calibrate (home + realign) | absolute / relative move, stop, sync position, set-zero, clear-stall |
| Writes | slot names, focus offsets, ARGB colors, friendly / Bluetooth names, config, factory reset | beep / backlash / reverse / speed / max-step, dew heater, stall detection, USB-power budget, names, factory reset |
| Backends | darwin (IOKit/cgo), linux (hidraw, pure Go), windows (SetupAPI, pure Go) | same |
| Tests | `go test -race ./oasisfw/` over a fake transport | `go test -race ./oasisfoc/` over a fake transport |

### Hardware-validated

- **Wheel:** identity/version reply, config read-modify-write, and the per-slot focus
  offset / color tables (signed int32 round-trips; colors are `0xAARRGGBB` ARGB).
- **Focuser:** reads, motion and index operations, names, the full config write
  surface (part-1 beep/backlash/reverse/speed/max-step and part-2 heater/stall/USB
  power), and the internal-temperature Beta curve.

### Still open

- Wheel config-block field offsets beyond the confirmed `mask`/speed/autorun/Bluetooth/turbo
  bytes.
- Temperature scaling and the exact size of the device's maximum name field (the driver
  caps to one report as a safe upper bound).
- The focuser's `FactoryReset` path is implemented but has not been exercised on
  hardware.

## Layout

```
oasisfw/ , oasisfoc/
  oasisfw.go / oasisfoc.go   device logic + HID interrupt framing (pure Go, all platforms)
  transport.go               the Transport interface (the seam) + DeviceInfo + USB IDs
  transport_darwin.go        macOS   — IOKit (cgo): SetReport(Output) + input-report callback
  transport_linux.go         Linux   — hidraw via raw O_NONBLOCK read/write (pure Go)
  transport_windows.go       Windows — SetupAPI + overlapped ReadFile/WriteFile (pure Go)
  transport_stub.go          other   — compile-only
  *_test.go                  protocol tests over a fake Transport (no hardware, no cgo)
cmd/oasisfwprobe/            CLI to open / inspect / drive a wheel
cmd/oasisfocprobe/           CLI to open / inspect / drive a focuser
```

## Build & test

```sh
# unit tests (no hardware needed)
go test -race ./...

# cross-build a probe for a deploy target (e.g. Raspberry Pi); linux/windows are pure Go
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o oasisfwprobe  ./cmd/oasisfwprobe
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o oasisfocprobe ./cmd/oasisfocprobe
```

On Linux, opening the device needs a udev rule so the service user can reach the
hidraw node:

```
KERNEL=="hidraw*", ATTRS{idVendor}=="338f", MODE="0660", TAG+="uaccess"
```

## Probes

Both CLIs default to a read-only dump and take flags to drive the device. **Setters
marked persistent write to the device's flash; `factoryreset` restores defaults.**

```sh
# filter wheel
oasisfwprobe                     # read-only: identity, status, config, names, offsets, colors
oasisfwprobe -goto 2             # move to slot 2 (0-based), then watch it settle
oasisfwprobe -calibrate          # home / realign
oasisfwprobe -setfocus 2:-150    # PERSISTENT: slot 2 focus offset
oasisfwprobe -setcolor 0:00ff00  # PERSISTENT: slot 0 color (RRGGBB hex)
oasisfwprobe -setslotname 1:Ha   # PERSISTENT: slot 1 name

# focuser
oasisfocprobe                    # read-only: identity, status, temps, config
oasisfocprobe -moveto 12000      # absolute move, then watch it settle
oasisfocprobe -move 1:500        # relative move dir:steps
oasisfocprobe -in 500 / -out 500 # move IN / OUT by N steps
oasisfocprobe -stop              # halt motion
oasisfocprobe -watch             # poll position + moving repeatedly
```

## Validating against new hardware

1. Run the probe read-only and confirm identity, status, position, and temperature
   look sane.
2. Drive a motion command (`-goto` / `-moveto`) and watch it settle.
3. Exercise the persistent setters with their built-in read-back checks (the probes
   write, then read the value back) and confirm the round-trip.
