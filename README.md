# oasis-astro

Go drivers for **Astroasis Oasis** USB accessories — talking each device's HID
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


| | Filter wheel (`oasisfw`) | Focuser (`oasisfoc`) |
|---|---|---|
| USB ID | VID `0x338F` / PID `0x0FE0` | VID `0x338F` / PID `0xA0F0` |
| Identity | model, serial, hardware/firmware/protocol versions, firmware build date | model, serial, hardware/firmware/protocol versions, firmware build date |
| Read surface | status (position, state, temperature), slot count, per-slot names / focus offsets / colors, config | status (position, moving, internal + external temperature), config + extended config |
| Motion | go-to slot, calibrate (home + realign) | absolute / relative move, stop, sync position, set-zero, clear-stall |
| Writes | slot names, focus offsets, ARGB colors, friendly / Bluetooth names, config, factory reset | beep / backlash / reverse / speed / max-step, dew heater, stall detection, USB-power budget, names, factory reset |
| Backends | darwin (IOKit/cgo), linux (hidraw, pure Go), windows (SetupAPI, pure Go) | same |
| Tests | `go test -race ./oasisfw/` over a fake transport | `go test -race ./oasisfoc/` over a fake transport |

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
